package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go-imap/go1/imap"
	"code.google.com/p/go.text/encoding/charmap"
	"code.google.com/p/go.text/transform"
)

func sortByThreads(mails []*ParsedMail) [][]*ParsedMail {
	byThread := make(map[uint64][]*ParsedMail)
	for _, m := range mails {
		byThread[m.Thrid] = append(byThread[m.Thrid], m)
	}
	result := make([][]*ParsedMail, 0, len(byThread))
	for _, thread := range byThread {
		result = append(result, thread)
	}
	return result

}

// from http://encoding.spec.whatwg.org/#names-and-labels
var knownCharsets = []*charset{
	// utf-8
	&charset{
		names:   []string{"unicode-1-1-utf-8", "utf-8", "utf8", "" /*default*/},
		decoder: transform.Nop,
	},
	// windows-1252
	&charset{
		names: []string{
			"unicode-1-1-utf-8",
			"ascii",
			"cp1252",
			"cp819",
			"csisolatin1",
			"ibm819",
			"iso-8859-1",
			"iso-ir-100",
			"iso8859-1",
			"iso88591",
			"iso_8859-1",
			"iso_8859-1:1987",
			"l1",
			"latin1",
			"us-ascii",
			"windows-1252",
			"x-cp1252",
		},
		decoder: charmap.Windows1252.NewDecoder(),
	},
}

type charset struct {
	names   []string
	decoder transform.Transformer
}

func getReader(charset string, r io.Reader) (io.Reader, error) {
	charset = strings.ToLower(charset)
	for _, m := range knownCharsets {
		for _, name := range m.names {
			if charset == name {
				return transform.NewReader(r, m.decoder), nil
			}
		}
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
	Body   string
	Thrid  uint64
}

func parseMail(b []byte) (*ParsedMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("failed to parse message: " + err.Error())
	}
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
					log.Println(msg.Header.Get("Subject"))
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
	return &ParsedMail{Header: msg.Header, Body: string(body)}, nil
}

func fetch(user, authToken string) ([]*ParsedMail, error) {
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
	parsed := make([]*ParsedMail, len(cmd.Data))
	for i, rsp := range cmd.Data {
		p, err := parseMail(imap.AsBytes(rsp.MessageInfo().Attrs["BODY[]"]))
		if err != nil {
			return nil, err
		}
		thridStr := imap.AsString(rsp.MessageInfo().Attrs["X-GM-THRID"])
		thrid, err := strconv.ParseUint(thridStr, 10, 64)
		if err != nil {
			return nil, errors.New("bad value for X-GM-THRID: " + thridStr)
		}
		p.Thrid = thrid
		parsed[i] = p
	}
	return parsed, nil
}
