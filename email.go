package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	unsafeRand "math/rand"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"code.google.com/p/go.net/html/charset"
	"github.com/mxk/go-imap/imap"
)

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

type ParsedMail struct {
	Header    mail.Header
	Body      template.HTML
	BodyLink  string
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
		body, err = ioutil.ReadAll(r)
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
		parsed.Body = template.HTML(template.HTMLEscapeString(string(body)))
	} else {
		parsed.Body = template.HTML(template.HTMLEscapeString(string(body)))
	}
	key := genKey()
	saveFragment(key, string(body))
	parsed.BodyLink = "fragment?key=" + key
	return parsed, nil
}

func genKey() string {
	b := make([]byte, 64)
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

func archive(c *imap.Client, thread Thread) error {
	// The archive operation is removing the \Inbox label.
	// But when viewed in INBOX, the messages don't have that label.
	// We have to switch to [Gmail]/All Mail.
	if len(thread) == 0 { // Save some round trips.
		return nil
	}
	var set imap.SeqSet
	for _, uid := range thread {
		set.AddNum(uid)
	}
	cmd, err := imap.Wait(c.UIDFetch(&set, "X-GM-MSGID"))
	if err != nil {
		return err
	}
	msgids := make([]string, len(cmd.Data))
	for i, rsp := range cmd.Data {
		msgids[i] = "X-GM-MSGID " + imap.AsString(rsp.MessageInfo().Attrs["X-GM-MSGID"])
	}
	prev := c.Mailbox.Name
	c.Select("[Gmail]/All Mail", false)
	defer c.Select(prev, true)
	cmd, err = imap.Wait(c.UIDSearch(strings.Repeat("OR ", len(msgids)-1) + strings.Join(msgids, " ")))
	if err != nil {
		log.Println(err)
		return ErrBadConnection
	}
	if len(cmd.Data) == 0 {
		log.Println("no responses from completed seach")
		return ErrBadConnection
	}
	set.Clear()
	set.AddNum(cmd.Data[0].SearchResults()...)
	_, err = imap.Wait(c.UIDStore(&set, "-X-GM-LABELS", `\Inbox`))
	return err
}

func fetch(c *imap.Client, thread Thread) ([]*ParsedMail, error) {
	var set imap.SeqSet
	for _, uid := range thread {
		set.AddNum(uid)
	}
	cmd, err := imap.Wait(c.UIDFetch(&set, "BODY[]", "X-GM-MSGID"))
	if err != nil {
		return nil, ErrBadConnection
	}
	parsed := make([]*ParsedMail, len(cmd.Data))
	for i, rsp := range cmd.Data {
		p, err := parseMail(imap.AsBytes(rsp.MessageInfo().Attrs["BODY[]"]))
		if err != nil {
			return nil, err
		}
		link, err := gmailLink(imap.AsString(rsp.MessageInfo().Attrs["X-GM-MSGID"]))
		if err != nil {
			return nil, err
		}
		p.GmailLink = link
		parsed[i] = p
	}
	return parsed, nil
}
