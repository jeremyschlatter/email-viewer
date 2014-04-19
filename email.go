package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
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
	byThread := make([][]*ParsedMail, 0, len(mails))
	for _, m := range mails {
		for i := range byThread {
			if byThread[i][0].Thrid == m.Thrid {
				byThread[i] = append(byThread[i], m)
				continue Outer
			}
		}
		byThread = append(byThread, []*ParsedMail{m})
	}
	return byThread

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

func parseContent(r io.Reader, contentType string) ([]byte, bool) {
	media, params, _ := mime.ParseMediaType(contentType)
	switch {
	case media == "text/hmtl", media == "text/plain":
		r, err := getReader(params["charset"], r)
		if err != nil {
			return nil, err == nil
		}
		body, err := ioutil.ReadAll(r)
		return body, err == nil
	case strings.HasPrefix(media, "multipart"):
		mp := multipart.NewReader(r, params["boundary"])
		for {
			part, err := mp.NextPart()
			if err != nil {
				return nil, false
			}
			if body, ok := parseContent(part, part.Header.Get("Content-Type")); ok {
				return body, true
			}
		}
	}
	return nil, false
}

func parseMail(b []byte) (*ParsedMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("failed to parse message: " + err.Error())
	}
	body, ok := parseContent(msg.Body, msg.Header.Get("Content-Type"))
	if !ok {
		return nil, fmt.Errorf("failed to find recognized content in this email: '%s'", msg.Header.Get("Subject"))
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
