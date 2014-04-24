package main

import (
	"testing"

	"github.com/mxk/go-imap/imap"
	"github.com/mxk/go-imap/mock"
)

func boilerplate(T *testing.T) (*mock.T, *imap.Client) {
	t := mock.Server(T,
		`S: * OK [CAPABILITY IMAP4rev1] Server ready`,
		`C: A1 LOGIN "joe" "password"`,
		`S: A1 OK LOGIN completed`,
		`C: A2 CAPABILITY`,
		`S: * CAPABILITY IMAP4rev1`,
		`S: A2 OK Thats all she wrote!`,
		`C: A3 EXAMINE "MockBox"`,
		`S: A3 OK [READ-ONLY] "MockBox" selected. (Success)`,
	)
	c, err := t.Dial()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Login("joe", "password")
	if err != nil {
		t.Fatal(err)
	}
	c.Select("MockBox", true)
	return t, c
}

func EqualThreads(t1, t2 []Thread) bool {
	if len(t1) != len(t2) {
		return false
	}
	for i := range t1 {
		tt1, tt2 := t1[i], t2[i]
		if len(tt1) != len(tt2) {
			return false
		}
		for j := range tt1 {
			if tt1[j] != tt2[j] {
				return false
			}
		}
	}
	return true
}

func TestFetchThreads(T *testing.T) {
	cases := []struct {
		msgs    []interface{}
		threads []Thread
	}{
		{
			[]interface{}{
				`S: * 1 FETCH (X-GM-THRID 1000000000000000000 UID 100)`,
				`S: * 2 FETCH (X-GM-THRID 1000000000000000001 UID 101)`,
			},
			[]Thread{
				Thread{100},
				Thread{101},
			},
		},
		{
			// empty inbox
			[]interface{}{},
			[]Thread{},
		},
		{
			[]interface{}{
				`S: * 1 FETCH (X-GM-THRID 1000000000000000001 UID 100)`,
				`S: * 2 FETCH (X-GM-THRID 1000000000000000001 UID 101)`,
				`S: * 3 FETCH (X-GM-THRID 1000000000000000001 UID 102)`,
			},
			[]Thread{
				Thread{100, 101, 102},
			},
		},
		{
			[]interface{}{
				`S: * 1 FETCH (X-GM-THRID 1000000000000000001 UID 100)`,
				`S: * 2 FETCH (X-GM-THRID 1000000000000000003 UID 101)`,
				`S: * 3 FETCH (X-GM-THRID 1000000000000000001 UID 102)`,
				`S: * 4 FETCH (X-GM-THRID 1000000000000000002 UID 103)`,
				`S: * 5 FETCH (X-GM-THRID 1000000000000000002 UID 104)`,
				`S: * 6 FETCH (X-GM-THRID 1000000000000000001 UID 105)`,
			},
			[]Thread{
				Thread{100, 102, 105},
				Thread{101},
				Thread{103, 104},
			},
		},
	}
	for _, tt := range cases {
		t, c := boilerplate(T)
		scripts := make([]interface{}, 0, len(tt.msgs)+2)
		scripts = append(scripts, `C: A4 FETCH 1:* (X-GM-THRID UID)`)
		scripts = append(scripts, tt.msgs...)
		scripts = append(scripts, `S: A4 OK Success`)
		t.Script(scripts...)
		threads, err := getThreads(c)
		t.Join(err)
		if !EqualThreads(tt.threads, threads) {
			t.Errorf("For:\n%v\nGot:\n%v\nWant:\n%v\n", tt.msgs, threads, tt.threads)
		}
	}
}

func TestArchiveHappy(T *testing.T) {
	t, c := boilerplate(T)
	t.Script(
		`C: A4 UID FETCH 40477:40478,40491 (X-GM-MSGID)`,
		`S: * 1 FETCH (X-GM-MSGID 1466243285656103619 UID 40477)`,
		`S: * 2 FETCH (X-GM-MSGID 1466243299960225271 UID 40478)`,
		`S: * 13 FETCH (X-GM-MSGID 1466285707386133302 UID 40491)`,
		`S: A4 OK Success`,
		`C: A5 SELECT "[Gmail]/All Mail"`,
		`S: A5 OK [READ-WRITE] [Gmail]/All Mail selected. (Success)`,
		`C: A6 UID SEARCH CHARSET UTF-8 OR OR X-GM-MSGID 1466243285656103619 X-GM-MSGID 1466243299960225271 X-GM-MSGID 1466285707386133302`,
		`S: * SEARCH 110720 110721 110829`,
		`S: A6 OK SEARCH completed (Success)`,
		`C: A7 UID STORE 110720:110721,110829 -X-GM-LABELS \Inbox`,
		`S: * 56074 FETCH (X-GM-LABELS ("\\Important") UID 110720)`,
		`S: * 56075 FETCH (X-GM-LABELS ("\\Important") UID 110721)`,
		`S: * 56089 FETCH (X-GM-LABELS ("\\Important" "\\Sent") UID 110829)`,
		`S: A7 OK Success`,
		`C: A8 EXAMINE "MockBox"`,
		`S: A8 OK [READ-ONLY] "MockBox" selected. (Success)`,
	)
	t.Join(archive(c, Thread{40477, 40478, 40491}))
}

func TestArchiveEmpty(T *testing.T) {
	t, c := boilerplate(T)
	t.Join(archive(c, Thread{}))
}

func TestArchiveSingle(T *testing.T) {
	t, c := boilerplate(T)
	t.Script(
		`C: A4 UID FETCH 40477 (X-GM-MSGID)`,
		`S: * 1 FETCH (X-GM-MSGID 1466243285656103619 UID 40477)`,
		`S: A4 OK Success`,
		`C: A5 SELECT "[Gmail]/All Mail"`,
		`S: A5 OK [READ-WRITE] [Gmail]/All Mail selected. (Success)`,
		`C: A6 UID SEARCH CHARSET UTF-8 X-GM-MSGID 1466243285656103619`,
		`S: * SEARCH 110721`,
		`S: A6 OK SEARCH completed (Success)`,
		`C: A7 UID STORE 110721 -X-GM-LABELS \Inbox`,
		`S: * 56075 FETCH (X-GM-LABELS ("\\Important") UID 110721)`,
		`S: A7 OK Success`,
		`C: A8 EXAMINE "MockBox"`,
		`S: A8 OK [READ-ONLY] "MockBox" selected. (Success)`,
	)
	t.Join(archive(c, Thread{40477}))
}
