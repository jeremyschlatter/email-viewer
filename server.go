package main

import (
	"flag"
	"html/template"
	"log"
	"net/http"
)

var httpAddr = flag.String("http", ":8080", "port to listen to")

type Data struct {
	EmailAddress string
	Messages     []ParsedMail
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	homeTemplate, err := template.ParseFiles("email.html")
	if err != nil {
		log.Println(err)
		http.Error(w, "whoops, something broke. sorry!", http.StatusInternalServerError)
		return
	}
	var data Data
	if u, t := r.FormValue("user"), r.FormValue("token"); u != "" && t != "" {
		data.EmailAddress = u
		m, err := fetch(u, t)
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

func main() {
	flag.Parse()
	template.Must(template.ParseFiles("email.html"))
	http.HandleFunc("/", homeHandler)
	log.Println("listening at", *httpAddr)
	log.Println(http.ListenAndServe(*httpAddr, Log(http.DefaultServeMux)))
}
