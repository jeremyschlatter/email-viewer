package main

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/mail"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"code.google.com/p/go-imap/go1/imap"
	"code.google.com/p/go.net/html/charset"
)

func sortByThreads(mails []*ParsedMail) [][]*ParsedMail {
	byThread := make([][]*ParsedMail, 0, len(mails))
Outer:
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
	Header    mail.Header
	Body      template.HTML
	Thrid     uint64
	GmailLink string
}

func sanitizeHTML(r io.Reader) ([]byte, error) {
	cmd := exec.Command("js", "sanitize.js")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	if _, err = io.Copy(stdin, r); err != nil {
		return nil, err
	}
	if err = stdin.Close(); err != nil {
		return nil, err
	}
	san, err := ioutil.ReadAll(stdout)
	if err != nil {
		return nil, err
	}
	return san, cmd.Wait()
}

var textHTML = "text/html"
var textPlain = "text/plain"

func parseContent(r io.Reader, contentType string) (body []byte, foundType string, err error) {
	media, params, _ := mime.ParseMediaType(contentType)
	switch {
	case media == textHTML, media == textPlain:
		r, err = charset.NewReader(r, params["charset"])
		if err != nil {
			return nil, "", err
		}
		if media == textHTML {
			body, err = sanitizeHTML(r)
		} else {
			body, err = ioutil.ReadAll(r)
		}
		return body, media, err
	case strings.HasPrefix(media, "multipart"):
		mp := multipart.NewReader(r, params["boundary"])
		var tmp []byte
		for {
			part, err := mp.NextPart()
			if err != nil {
				return nil, "", err
			}
			if tmp, foundType, err = parseContent(part, part.Header.Get("Content-Type")); err == nil {
				body = tmp
				if foundType == textHTML {
					break
				} // else keep fishing for a text/html part
			}
		}
		if body == nil {
			foundType = ""
		}
		return body, foundType, nil
	}
	return nil, "", nil
}

func parseMail(b []byte) (*ParsedMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("failed to parse message: " + err.Error())
	}
	body, foundType, err := parseContent(msg.Body, msg.Header.Get("Content-Type"))
	if err != nil {
		body = []byte("failed to parse content. view in gmail")
	}
	parsed := &ParsedMail{Header: msg.Header}
	if foundType == textHTML {
		parsed.Body = template.HTML(body) // parseContent sanitizes text/html
	} else {
		parsed.Body = template.HTML(template.HTMLEscapeString(string(body)))
	}
	return parsed, nil
}

func gmailLink(s string) (string, error) {
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return "", errors.New("bad value for X-GM-THRID: " + s)
	}
	return fmt.Sprintf("https://mail.google.com/mail/u/0/#inbox/%s", strconv.FormatUint(u, 16)), nil
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
	set, _ := imap.NewSeqSet("1")
	cmd, err := imap.Wait(c.Fetch(set, "BODY.PEEK[]", "X-GM-THRID", "X-GM-MSGID"))
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
		link, err := gmailLink(imap.AsString(rsp.MessageInfo().Attrs["X-GM-MSGID"]))
		if err != nil {
			return nil, err
		}
		p.GmailLink = link
		parsed[i] = p
	}
	return parsed, nil
}
