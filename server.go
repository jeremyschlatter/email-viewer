package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"code.google.com/p/goauth2/oauth"
	"code.google.com/p/google-api-go-client/plus/v1"
	"github.com/gorilla/securecookie"
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

func checkInitError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func init() {
	file, err := os.Open("server-config.json")
	checkInitError(err)
	var c serverConfig
	checkInitError(json.NewDecoder(file).Decode(&c))
	config.ClientId = c.ClientID
	config.ClientSecret = c.ClientSecret
	hashKey, err := base64.StdEncoding.DecodeString(c.CookieHashKey)
	checkInitError(err)
	blockKey, err := base64.StdEncoding.DecodeString(c.CookieBlockKey)
	checkInitError(err)
	if len(hashKey) != 64 || len(blockKey) != 32 {
		log.Fatal("bad key lengths")
	}
	s = securecookie.New(hashKey, blockKey)
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
	homeTemplate, err := template.ParseFiles("email.html")
	if err != nil {
		log.Println(err)
		http.Error(w, "whoops, something broke. sorry!", http.StatusInternalServerError)
		return
	}
	data := Data{AuthURL: config.AuthCodeURL("")}
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
		m, err := fetch(c, threads[0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Messages = m
		data.Threads = threads
	}
	if err = homeTemplate.Execute(w, data); err != nil {
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

func main() {
	flag.Parse()
	template.Must(template.ParseFiles("email.html"))
	http.HandleFunc("/", homeHandler)
	http.Handle("/static/", http.StripPrefix("/static", http.FileServer(justFiles("static"))))
	http.HandleFunc("/fragment", fragmentHandler)
	log.Println("listening at", *httpAddr)
	log.Println(http.ListenAndServe(*httpAddr, Log(http.DefaultServeMux)))
}
