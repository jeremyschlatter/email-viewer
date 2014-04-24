package main

import (
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"code.google.com/p/goauth2/oauth"
	"code.google.com/p/google-api-go-client/plus/v1"
	"github.com/boltdb/bolt"
)

var httpAddr = flag.String("http", ":8080", "port to listen to")

var tokenBucket = []byte("tokens")

type credential struct {
	ClientID     string
	ClientSecret string
}

var db *bolt.DB

func initDB() {
	var err error
	db, err = bolt.Open("emails.db", 0600)
	if err != nil {
		log.Fatalln(err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(tokenBucket)
		return err
	})
	if err != nil && err != bolt.ErrBucketExists {
		log.Fatalln(err)
	}
}

func init() {
	file, err := os.Open("credentials.json")
	if err != nil {
		log.Fatalln(err)
	}
	var c credential
	if err = json.NewDecoder(file).Decode(&c); err != nil {
		log.Fatalln(err)
	}
	config.ClientId = c.ClientID
	config.ClientSecret = c.ClientSecret
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

func getToken(user string, t *oauth.Token) error {
	var tokenBytes []byte
	db.View(func(tx *bolt.Tx) error {
		tokenBytes = tx.Bucket(tokenBucket).Get([]byte(user))
		return nil
	})
	return json.Unmarshal(tokenBytes, t)
}

func saveToken(user string, t *oauth.Token) error {
	tokenBytes, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(tokenBucket).Put([]byte(user), tokenBytes)
	})
}

func leakyLog(w http.ResponseWriter, err error) {
	log.Println(err)
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	homeTemplate, err := template.ParseFiles("email.html")
	if err != nil {
		log.Println(err)
		http.Error(w, "whoops, something broke. sorry!", http.StatusInternalServerError)
		return
	}
	data := Data{AuthURL: config.AuthCodeURL("")}
	var token *oauth.Token
	var user string
	if c, err := r.Cookie("user"); err == nil {
		token = new(oauth.Token)
		user = c.Value
		if err = getToken(c.Value, token); err != nil {
			leakyLog(w, err)
			return
		}
		if token.Expired() {
			t := &oauth.Transport{Config: config, Token: token}
			if err = t.Refresh(); err != nil {
				leakyLog(w, err)
				return
			}
		}
	} else if code := r.FormValue("code"); code != "" {
		t := &oauth.Transport{Config: config}
		t.Exchange(code)
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
		saveToken(user, t.Token)
		token = t.Token
		http.SetCookie(w, &http.Cookie{Name: "user", Value: user, Secure: true})
	}
	if token != nil {
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
	homeTemplate.Execute(w, data)
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

func main() {
	flag.Parse()
	initDB()
	defer db.Close()
	template.Must(template.ParseFiles("email.html"))
	http.HandleFunc("/", homeHandler)
	http.Handle("/static/", http.StripPrefix("/static", http.FileServer(justFiles("static"))))
	log.Println("listening at", *httpAddr)
	log.Println(http.ListenAndServe(*httpAddr, Log(http.DefaultServeMux)))
}
