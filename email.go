package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go-imap/go1/imap"
	"code.google.com/p/go.text/encoding/charmap"
	"code.google.com/p/go.text/transform"
)

var user = flag.String("user", "", "your gmail address")
var token = flag.String("token", "", "oauth access token. you can get one at https://developers.google.com/oauthplayground/")

func main() {
	imap.DefaultLogger = log.New(os.Stdout, "", 0)
	imap.DefaultLogMask = imap.LogNone
	flag.Parse()
	if *user == "" || *token == "" {
		flag.Usage()
		return
	}
	mails, err := fetch(*user, *token)
	if err != nil {
		fmt.Println("Error in my fetch: ", err)
	}
	byThread := make(map[uint64][]ParsedMail)
	for _, m := range mails {
		byThread[m.Thrid] = append(byThread[m.Thrid], m)
	}
	for k, ms := range byThread {
		fmt.Println("Thread", k)
		for _, m := range ms {
			fmt.Println("    ", m.Header.Get("Subject"))
		}
	}
}

// https://stackoverflow.com/questions/6002619/unmarshal-an-iso-8859-1-xml-input-in-go
func isCharset(charset string, names []string) bool {
	charset = strings.ToLower(charset)
	for _, n := range names {
		if charset == strings.ToLower(n) {
			return true
		}
	}
	return false
}

func IsCharsetISO88591(charset string) bool {
	// http://www.iana.org/assignments/character-sets
	// (last updated 2010-11-04)
	names := []string{
		// Name
		"ISO_8859-1:1987",
		// Alias (preferred MIME name)
		"ISO-8859-1",
		// Aliases
		"iso-ir-100",
		"ISO_8859-1",
		"latin1",
		"l1",
		"IBM819",
		"CP819",
		"csISOLatin1",
	}
	return isCharset(charset, names)
}

func IsCharsetUTF8(charset string) bool {
	names := []string{
		"UTF-8",
	}
	return isCharset(charset, names)
}

func getReader(charset string, r io.Reader) (io.Reader, error) {
	switch {
	case IsCharsetUTF8(charset):
		return r, nil
	case IsCharsetISO88591(charset):
		// Windows-1252 is slightly different from ISO-8859-1, but only
		// because it replaces C1 control codes with displayable characters.
		// https://en.wikipedia.org/wiki/Windows-1252
		return transform.NewReader(r, charmap.Windows1252.NewDecoder()), nil
	}
	return nil, errors.New("Unexpected charset: " + charset)
}

type oauthSASL struct {
	user, token string
}

func (o oauthSASL) Start(s *imap.ServerInfo) (string, []byte, error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", o.user, o.token)), nil
}

func (o oauthSASL) Next(challenge []byte) ([]byte, error) {
	return nil, errors.New("Challenge shouldn't be issued.")
}

type ParsedMail struct {
	Header mail.Header
	Body   []byte
	Thrid  uint64
}

func fetch(user, authToken string) ([]ParsedMail, error) {
	c, err := imap.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		return nil, err
	}
	defer c.Logout(15 * time.Second)
	if _, err = c.Auth(oauthSASL{user, authToken}); err != nil {
		return nil, err
	}
	c.Select("INBOX", true)
	set, _ := imap.NewSeqSet("1:*")
	cmd, err := imap.Wait(c.Fetch(set, "BODY.PEEK[]", "X-GM-THRID"))
	if err != nil {
		return nil, errors.New("fetch error: " + err.Error())
	}
	parsed := make([]ParsedMail, len(cmd.Data))
	for i, rsp := range cmd.Data {
		msgBytes := imap.AsBytes(rsp.MessageInfo().Attrs["BODY[]"])
		if msg, _ := mail.ReadMessage(bytes.NewReader(msgBytes)); msg != nil {
			var body []byte
			if media, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type")); strings.HasPrefix(media, "multipart") {
				b := params["boundary"]
				if b == "" {
					return nil, errors.New("multipart message with no boundary specified")
				}
				var buf bytes.Buffer
				mp := multipart.NewReader(msg.Body, b)
				var partsList []string
				for {
					part, err := mp.NextPart()
					if err == io.EOF {
						if buf.Len() == 0 {
							return nil, errors.New("no text/html or text/plain parts here: " + strings.Join(partsList, "; "))
						}
						break
					}
					if err != nil {
						return nil, errors.New("error reading multipart message: " + err.Error())
					}
					media, params, _ = mime.ParseMediaType(part.Header.Get("Content-Type"))
					if media == "text/html" || media == "text/plain" {
						buf.Reset()
						r, err := getReader(params["charset"], part)
						if err != nil {
							return nil, err
						}
						io.Copy(&buf, r)
						if media == "text/html" {
							break
						}
					}
					partsList = append(partsList, media)
				}
				body = buf.Bytes()
			} else {
				body, err = ioutil.ReadAll(msg.Body)
				if err != nil {
					return nil, errors.New("error reading message body: " + err.Error())
				}
			}
			thrid, _ := strconv.ParseUint(imap.AsString(rsp.MessageInfo().Attrs["X-GM-THRID"]), 10, 64)
			parsed[i] = ParsedMail{Header: msg.Header, Body: body, Thrid: thrid}
		} else {
			return nil, errors.New("failed to parse message")
		}
	}
	return parsed, nil
}
