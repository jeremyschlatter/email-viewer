package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	unsafeRand "math/rand"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.google.com/p/go.net/html/charset"
	"github.com/ThomsonReutersEikon/mailstrip"
	"github.com/jeremyschlatter/shellout"
	"github.com/mxk/go-imap/imap"
)

func sanitized(r io.Reader) (io.Reader, error) {
	return shellout.Start(r, "ruby", "sanitize.rb")
}

func init() {
	imap.DefaultLogger = log.New(os.Stdout, "", 0)
	imap.DefaultLogMask = imap.LogNone
	//imap.DefaultLogMask = imap.LogConn | imap.LogRaw
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

// The smtp.Auth and imap.SASL interfaces are very slightly different, so we need two very slightly different types.
type smtpAuth struct {
	user, token string
}

func (o smtpAuth) Start(s *smtp.ServerInfo) (string, []byte, error) {
	return "XOAUTH2", []byte(fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", o.user, o.token)), nil
}

func (o smtpAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		fmt.Println(string(fromServer))
		return nil, errors.New("Unexpected server message.")
	}
	return nil, nil
}

func Strip(s string) string {
	return mailstrip.Parse(s).String()
}

type ParsedMail struct {
	Header          mail.Header
	TextBody        string
	From            string
	BodyLink        string
	GmailLink       string
	Recipients      []string
	NamedRecipients []string
	Thrid           string
}

var textHTML = "text/html"
var textPlain = "text/plain"

func parseContent(r io.Reader, contentType string) (htmlBody, textBody string, err error) {
	media, params, _ := mime.ParseMediaType(contentType)
	switch {
	case media == textHTML, media == textPlain:
		r, err = charset.NewReader(r, params["charset"])
		if err != nil {
			return "", "", err
		}
		if media == textHTML {
			r, err = sanitized(r)
			if err != nil {
				return "", "", err
			}
		}
		body, err := ioutil.ReadAll(r)
		s := string(body)
		if media == textHTML {
			return s, "", err
		}
		return "", s, err
	case strings.HasPrefix(media, "multipart"):
		mp := multipart.NewReader(r, params["boundary"])
		for {
			part, err := mp.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", "", err
			}
			if tmpHTML, tmpText, err := parseContent(part, part.Header.Get("Content-Type")); err == nil {
				if tmpHTML != "" {
					htmlBody = tmpHTML
				}
				if tmpText != "" {
					textBody = tmpText
				}
				if htmlBody != "" && textBody != "" {
					break
				}
			}
		}
	}
	return htmlBody, textBody, nil
}

func parseMail(b []byte, user string) (*ParsedMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return nil, errors.New("failed to parse message: " + err.Error())
	}
	htmlBody, textBody, err := parseContent(msg.Body, msg.Header.Get("Content-Type"))
	if err != nil {
		textBody = "failed to parse content. view in gmail"
		fmt.Println(err)
	}
	parsed := &ParsedMail{Header: msg.Header}
	parsed.TextBody = textBody
	if htmlBody != "" {
		key := genKey(64)
		saveFragment(key, Strip(htmlBody))
		parsed.BodyLink = "fragment?key=" + key
	}
	seen := make(map[string]bool)
	for _, f := range []string{"To", "From", "Cc"} {
		lst, err := msg.Header.AddressList(f)
		if err != nil {
			continue
		}
		for _, a := range lst {
			if !seen[a.Address] && (a.Address != user || f == "From") {
				seen[a.Address] = true
				parsed.Recipients = append(parsed.Recipients, a.Address)
				parsed.NamedRecipients = append(parsed.NamedRecipients, a.String())
			}
		}
	}
	a, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		parsed.From = msg.Header.Get("From")
	} else {
		parsed.From = fmt.Sprintf("%s <%s>", a.Name, a.Address)
	}
	return parsed, nil
}

func archive(c *imap.Client, thrid string) error {
	cmd, err := imap.Wait(c.UIDSearch("X-GM-THRID", thrid))
	if err != nil {
		log.Println(err)
		return ErrBadConnection
	}
	if len(cmd.Data) == 0 {
		return nil
	}
	var set imap.SeqSet
	set.AddNum(cmd.Data[0].SearchResults()...)
	_, err = imap.Wait(c.UIDStore(&set, "+FLAGS.SILENT", `(\Deleted)`))
	return err
}

func genKey(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(unsafeRand.Uint32())
		}
	}
	return base64.URLEncoding.EncodeToString(b)
}

var (
	fragments map[string]string
	mu        sync.Mutex
)

func saveFragment(key, value string) {
	if fragments == nil {
		fragments = make(map[string]string)
	}
	mu.Lock()
	fragments[key] = value
	mu.Unlock()
}

func getFragment(key string) string {
	mu.Lock()
	value := fragments[key]
	delete(fragments, key)
	mu.Unlock()
	return value
}

func gmailLink(s string) (string, error) {
	u, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return "", errors.New("bad value for X-GM-MSGID: " + s)
	}
	return fmt.Sprintf("https://mail.google.com/mail/u/0/#inbox/%s", strconv.FormatUint(u, 16)), nil
}

type Thread []uint32

func connect(user, authToken string) (*imap.Client, error) {
	c, err := imap.DialTLS("imap.gmail.com:993", nil)
	if err != nil {
		return nil, err
	}
	if _, err = c.Auth(oauthSASL{user, authToken}); err != nil {
		c.Logout(2 * time.Second)
		return nil, err
	}
	return c, nil
}

// Error while communicating with gmail.
var ErrBadConnection = errors.New("Encountered error while communicating with gmail")

func getThreads(c *imap.Client) ([]Thread, error) {
	set, err := imap.NewSeqSet("1:*")
	cmd, err := imap.Wait(c.Fetch(set, "X-GM-THRID", "UID"))
	if err != nil {
		fmt.Println(err)
		return nil, ErrBadConnection
	}
	var result []Thread
	seen := make(map[string]int)
	for _, rsp := range cmd.Data {
		thrid := imap.AsString(rsp.MessageInfo().Attrs["X-GM-THRID"])
		uid := imap.AsNumber(rsp.MessageInfo().Attrs["UID"])
		if i, ok := seen[thrid]; ok {
			result[i] = append(result[i], uid)
		} else {
			result = append(result, Thread{uid})
			seen[thrid] = len(result) - 1
		}
	}
	return result, nil
}

func fetch(c *imap.Client, user string, thread Thread) ([]*ParsedMail, error) {
	var set imap.SeqSet
	for _, uid := range thread {
		set.AddNum(uid)
	}
	cmd, err := imap.Wait(c.UIDFetch(&set, "BODY[]", "X-GM-MSGID", "X-GM-THRID"))
	if err != nil {
		return nil, ErrBadConnection
	}
	parsed := make([]*ParsedMail, len(cmd.Data))
	for i, rsp := range cmd.Data {
		p, err := parseMail(imap.AsBytes(rsp.MessageInfo().Attrs["BODY[]"]), user)
		if err != nil {
			return nil, err
		}
		link, err := gmailLink(imap.AsString(rsp.MessageInfo().Attrs["X-GM-MSGID"]))
		if err != nil {
			return nil, err
		}
		p.GmailLink = link
		p.Thrid = imap.AsString(rsp.MessageInfo().Attrs["X-GM-THRID"])
		parsed[i] = p
	}
	return parsed, nil
}
