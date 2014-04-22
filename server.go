package main

import (
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"net/http"
	"os"

	"code.google.com/p/goauth2/oauth"
)

var httpAddr = flag.String("http", ":8080", "port to listen to")

type credential struct {
	ClientID     string
	ClientSecret string
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
	Scope:       "https://mail.google.com/ email",
	AuthURL:     "https://accounts.google.com/o/oauth2/auth",
	TokenURL:    "https://accounts.google.com/o/oauth2/token",
	RedirectURL: "https://jeremyschlatter.com/quick-email",
}

type Data struct {
	EmailAddress string
	Messages     []*ParsedMail
	LoggedIn     bool
	AuthURL      string
}

type Person struct {
	Emails []struct {
		Value string `json:"value"`
		Type  string `json:"type"`
	} `json:"emails"`
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	homeTemplate, err := template.ParseFiles("email.html")
	if err != nil {
		log.Println(err)
		http.Error(w, "whoops, something broke. sorry!", http.StatusInternalServerError)
		return
	}
	var data Data
	data.AuthURL = config.AuthCodeURL("")
	if code := r.FormValue("code"); code != "" {
		t := &oauth.Transport{Config: config}
		t.Exchange(code)
		c := t.Client()
		resp, err := c.Get("https://www.googleapis.com/plus/v1/people/me")
		if err != nil {
			log.Println(err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		var p Person
		json.NewDecoder(resp.Body).Decode(&p)
		var user string
		for _, email := range p.Emails {
			if email.Type == "account" {
				user = email.Value
				break
			}
		}
		data.LoggedIn = true
		data.EmailAddress = user
		m, err := fetch(user, t.Token.AccessToken)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		threads := sortByThreads(m)
		data.Messages = threads[0]
		for _, thread := range threads {
			if len(thread) > 1 {
				data.Messages = thread
				break
			}
		}
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
	template.Must(template.ParseFiles("email.html"))
	http.HandleFunc("/", homeHandler)
	http.Handle("/static/", http.StripPrefix("/static", http.FileServer(justFiles("static"))))
	log.Println("listening at", *httpAddr)
	log.Println(http.ListenAndServe(*httpAddr, Log(http.DefaultServeMux)))
}
