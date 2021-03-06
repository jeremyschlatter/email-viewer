package main

import (
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/mail"
	"net/textproto"
	"os"
	"strings"
	"time"

	"code.google.com/p/goauth2/oauth"
	"code.google.com/p/google-api-go-client/plus/v1"
	"github.com/ThomsonReutersEikon/mailstrip"
	"github.com/gorilla/context"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	sendmail "github.com/jordan-wright/email"
)

var httpAddr = flag.String("http", ":8080", "port to listen to")

var tokenBucket = []byte("tokens")

type serverConfig struct {
	ClientID       string
	ClientSecret   string
	CookieHashKey  string
	CookieBlockKey string
}

var s *securecookie.SecureCookie
var store sessions.Store

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func init() {
	gob.Register(&ParsedMail{})
	file, err := os.Open("server-config.json")
	check(err)
	var c serverConfig
	check(json.NewDecoder(file).Decode(&c))
	config.ClientId = c.ClientID
	config.ClientSecret = c.ClientSecret
	hashKey, err := base64.StdEncoding.DecodeString(c.CookieHashKey)
	check(err)
	blockKey, err := base64.StdEncoding.DecodeString(c.CookieBlockKey)
	check(err)
	if len(hashKey) != 64 || len(blockKey) != 32 {
		log.Fatal("bad key lengths")
	}
	s = securecookie.New(hashKey, blockKey)
	fss := sessions.NewFilesystemStore("/home/ubuntu/fsstore", hashKey, blockKey)
	fss.MaxLength(1024 * 1024)
	store = fss
}

var config = &oauth.Config{
	Scope:          "https://mail.google.com/ email",
	AuthURL:        "https://accounts.google.com/o/oauth2/auth",
	TokenURL:       "https://accounts.google.com/o/oauth2/token",
	RedirectURL:    "https://jeremyschlatter.com/quick-email",
	AccessType:     "offline",
	ApprovalPrompt: "force",
}

type Data struct {
	EmailAddress string
	Messages     []*ParsedMail
	LoggedIn     bool
	AuthURL      string
	Threads      []Thread
	CheckValue   string
}

type Person struct {
	Emails []struct {
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"emails"`
}

func leakyLog(w http.ResponseWriter, err error) {
	log.Println(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func SetSecureCookie(w http.ResponseWriter, name string, value interface{}) error {
	encoded, err := s.Encode(name, value)
	if err == nil {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    encoded,
			HttpOnly: true,
			Secure:   true})
	}
	return err
}

func ReadSecureCookie(r *http.Request, name string, value interface{}) error {
	cookie, err := r.Cookie(name)
	if err != nil {
		return err
	}
	return s.Decode(name, cookie.Value, value)
}

func getSavedCreds(w http.ResponseWriter, r *http.Request) (user string, token *oauth.Token, ok bool) {
	if ReadSecureCookie(r, "user", &user) != nil {
		return "", nil, false
	}
	token = new(oauth.Token)
	if ReadSecureCookie(r, "token", token) != nil {
		return "", nil, false
	}
	if token.Expired() {
		t := &oauth.Transport{Config: config, Token: token}
		if t.Refresh() != nil {
			return "", nil, false
		}
		SetSecureCookie(w, "token", token)
	}
	return user, token, true
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	homeTemplate, err := template.New("home").
		Funcs(template.FuncMap{"mailstrip": Strip}).
		ParseFiles("email.html")
	if err != nil {
		log.Println(err)
		http.Error(w, "whoops, something broke. sorry!", http.StatusInternalServerError)
		return
	}
	data := Data{AuthURL: config.AuthCodeURL(""), CheckValue: genKey(16)}
	session, err := store.Get(r, "email-session")
	if err != nil {
		leakyLog(w, errors.New("Problem getting session: "+err.Error()))
		return
	}
	user, token, ok := getSavedCreds(w, r)
	if !ok && r.FormValue("code") != "" {
		t := &oauth.Transport{Config: config}
		t.Exchange(r.FormValue("code"))
		plusService, _ := plus.New(t.Client())
		person, err := plusService.People.Get("me").Do()
		if err != nil {
			leakyLog(w, err)
			return
		}
		for _, email := range person.Emails {
			if email.Type == "account" {
				user = email.Value
				break
			}
		}
		token = t.Token
		SetSecureCookie(w, "user", user)
		SetSecureCookie(w, "token", token)
		ok = true
	}
	if ok {
		data.LoggedIn = true
		data.EmailAddress = user
		c, err := connect(user, token.AccessToken)
		// TODO: Errors here are almost certainly either oauth failures which are my fault
		//       or temporary network problems connecting to gmail. Users should not see this.
		if err != nil {
			http.Error(w, "Error connecting to gmail", http.StatusServiceUnavailable)
			return
		}
		c.Select("INBOX", true)
		defer c.Logout(15 * time.Second)
		threads, err := getThreads(c)
		if err != nil {
			http.Error(w, "Error connecting to gmail", http.StatusServiceUnavailable)
			return
		}
		m, err := fetch(c, user, threads[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		session.Values["last-message"] = m[len(m)-1]
		session.Values["check-value"] = data.CheckValue
		if err := session.Save(r, w); err != nil {
			leakyLog(w, errors.New("Problem saving session: "+err.Error()))
		}
		data.Messages = m
		data.Threads = threads
	}
	if err = homeTemplate.ExecuteTemplate(w, "email.html", data); err != nil {
		log.Println(err)
	}
}

func Log(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s\n", r.RemoteAddr, r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

type justFiles string

func (s justFiles) Open(name string) (http.File, error) {
	f, err := http.Dir(s).Open(name)
	if err != nil {
		return nil, err
	}
	return neuteredReaddirFile{f}, nil
}

type neuteredReaddirFile struct {
	http.File
}

func (f neuteredReaddirFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, nil
}

func fragmentHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, getFragment(r.FormValue("key")))
}

func archiveHandler(w http.ResponseWriter, r *http.Request) {
	user, token, ok := getSavedCreds(w, r)
	if !ok {
		leakyLog(w, errors.New("not logged in"))
		return
	}
	c, err := connect(user, token.AccessToken)
	// TODO: Errors here are almost certainly either oauth failures which are my fault
	//       or temporary network problems connecting to gmail. Users should not see this.
	if err != nil {
		http.Error(w, "Error connecting to gmail", http.StatusServiceUnavailable)
		return
	}
	c.Select("INBOX", false)
	defer func() {
		c.Close(true)
		c.Logout(5 * time.Second)
	}()
	if err := archive(c, r.PostFormValue("thrid")); err != nil {
		log.Println(err)
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
	} else {
		http.Redirect(w, r, "/quick-email", http.StatusFound)
	}
}

func sendHandler(w http.ResponseWriter, r *http.Request) {
	user, token, ok := getSavedCreds(w, r)
	if !ok {
		leakyLog(w, errors.New("not logged in"))
		return
	}
	if err := r.ParseForm(); err != nil {
		leakyLog(w, errors.New("malformed request"))
		return
	}
	session, err := store.Get(r, "email-session")
	if err != nil {
		leakyLog(w, err)
		return
	}
	m := session.Values["last-message"].(*ParsedMail)
	if s, c := session.Values["check-value"].(string), r.FormValue("check"); s != c {
		leakyLog(w, errors.New("error processing your request"))
		log.Printf("Server check: %s, client check: %s\n", s, c)
		return
	}
	text := r.FormValue("mail-text")
	text += fmt.Sprintf("\n\n\n%s\n\n%s", theyWrote(m.Header), blockquote(m.TextBody))
	msg := &sendmail.Email{
		To:      r.Form["named-recipients"],
		From:    user,
		Subject: r.FormValue("subject"),
		Text:    []byte(text),
		Headers: textproto.MIMEHeader{},
	}
	if msgid := m.Header.Get("Message-ID"); msgid != "" {
		msg.Headers.Add("In-Reply-To", msgid)
		r := m.Header.Get("References")
		if r != "" {
			r += " "
		}
		r += msgid
		msg.Headers.Add("References", r)
	}
	err = msg.Send("smtp.gmail.com:587", smtpAuth{user, token.AccessToken})
	if err != nil {
		leakyLog(w, err)
		return
	}
	http.Redirect(w, r, "/quick-email", http.StatusFound)
}

func blockquote(s string) string {
	stripped := mailstrip.Parse(s).String()
	r := strings.Replace("\n"+stripped, "\n", "\n> ", -1)
	i := consume(s, stripped)
	if i < len(s) {
		for _, line := range strings.Split(s[i:], "\n") {
			if line == "" || line[0] == '>' {
				r += "\n>" + line
			} else {
				r += "\n> " + line
			}
		}
	}
	return r
}

func consume(s, prefix string) int {
	i := 0
	for j := 0; j < len(prefix); j++ {
		for ; s[i] != s[j]; i++ {
			if i == len(s)-1 {
				return -1
			}
		}
	}
	return i + 1
}

func theyWrote(h mail.Header) string {
	var r string
	t, err := h.Date()
	if err == nil {
		r = t.Format("On Mon, Jan 2, 2006 at 3:04 PM, ")
	}
	return r + h.Get("From") + " wrote:"
}

func main() {
	flag.Parse()
	http.HandleFunc("/", homeHandler)
	http.Handle("/static/", http.StripPrefix("/static", http.FileServer(justFiles("static"))))
	http.HandleFunc("/fragment", fragmentHandler)
	http.HandleFunc("/send", sendHandler)
	http.HandleFunc("/archive", archiveHandler)
	log.Println("listening at", *httpAddr)
	log.Println(http.ListenAndServe(*httpAddr, context.ClearHandler(Log(http.DefaultServeMux))))
}
