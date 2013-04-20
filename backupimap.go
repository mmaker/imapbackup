// backupimap dumps an entire IMAP account to a ZIP file.
//
package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code.google.com/p/go-imap/go1/imap"
)

var (
	server   = flag.String("server", "mail.autistici.org:143", "IMAP server address")
	username = flag.String("user", "", "Username")
	password = flag.String("password", "", "Password")
	output   = flag.String("outfile", "", "Output ZIP file name")

	mboxCh       = make(chan *imap.MailboxInfo, 5)
	msgCh        = make(chan *Message, 100)
	msgIdCounter = 0

	hostname string
)

const (
	concurrentConnections = 3
)

func init() {
	hostname, _ = os.Hostname()

	// We might need a very big buffer.
	imap.BufferSize = 1 << 20
}

type Message struct {
	Folder string
	Body   []byte
}

func Check(cmd *imap.Command, err error) *imap.Command {
	if err != nil {
		log.Fatal(err)
	}
	if _, err := cmd.Result(imap.OK); err != nil {
		log.Fatal("IMAP error: ", err)
	}
	return cmd
}

func Connect() *imap.Client {
	var err error
	var c *imap.Client
	if strings.HasSuffix(*server, ":993") {
		c, err = imap.DialTLS(*server, nil)
	} else {
		s := *server
		if strings.Index(*server, ":") < 0 {
			s = s + ":143"
		}
		c, err = imap.Dial(s)
	}
	if err != nil {
		log.Fatal(err)
	}

	if c.Caps["STARTTLS"] {
		Check(c.StartTLS(nil))
	}

	Check(c.Login(*username, *password))

	return c
}

func DownloadMailbox(c *imap.Client, mbox *imap.MailboxInfo) {
	name := mbox.Name
	if strings.HasPrefix(name, "INBOX/") {
		name = name[6:]
	}

	// Skip some unwanted mailboxes.
	if name == "dovecot.sieve" || name == "Spam" || name == "Trash" || name == "Junk" {
		return
	}

	c.Select(mbox.Name, true)
	if c.Mailbox == nil {
		log.Printf("Error selecting mailbox '%s'", mbox.Name)
		return
	}
	log.Printf("%s - %d messages", name, c.Mailbox.Messages)
	if c.Mailbox.Messages == 0 {
		return
	}

	set, _ := imap.NewSeqSet("")
	set.Add("1:*")

	cmd, _ := c.Fetch(set, "BODY[]")
	for cmd.InProgress() {
		c.Recv(-1)

		for _, resp := range cmd.Data {
			msg := Message{
				Folder: name,
				Body:   imap.AsBytes(resp.MessageInfo().Attrs["BODY[]"]),
			}
			msgCh <- &msg
		}
		cmd.Data = nil

		// We're not doing anything with server notices, just clear them.
		c.Data = nil
	}

	if resp, err := cmd.Result(imap.OK); err != nil {
		if err == imap.ErrAborted {
			log.Printf("Fetch command aborted")
		} else {
			log.Printf("Fetch error: %s", resp.Info)
		}
	}
}

func MboxDownloader() {
	var c *imap.Client
	for mbox := range mboxCh {
		if c == nil {
			c = Connect()
		}
		DownloadMailbox(c, mbox)
	}
	Close(c)
}

func GetMaildirFileName() string {
	msgIdCounter++
	return fmt.Sprintf("%d.%d_1.%s:2,S",
		time.Now().Unix(),
		msgIdCounter,
		hostname)
}

func MsgWriter() {
	msgCount := 0

	file, err := os.Create(*output)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	zw := zip.NewWriter(file)

	for msg := range msgCh {
		zf, err := zw.Create(filepath.Join(msg.Folder, "cur", GetMaildirFileName()))
		if err != nil {
			log.Fatal(err)
		}
		zf.Write(msg.Body)
		msgCount++
	}

	zw.Close()
	log.Printf("retrieved %d messages, output written to %s", msgCount, *output)
}

func ListMailboxes(c *imap.Client) {
	cmd := Check(c.List("", "*"))
	for _, response := range cmd.Data {
		mbox := response.MailboxInfo()
		mboxCh <- mbox
	}
}

func Close(c *imap.Client) {
	Check(c.Logout(30 * time.Second))
}

func Usage() {
	fmt.Fprintf(os.Stderr, "backupimap - backup your IMAP accounts to ZIP files\n\n")
	fmt.Fprintf(os.Stderr, "Usage: %s\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	flag.Usage = Usage
	flag.Parse()

	if *username == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "You must specify both --user and --password!")
		os.Exit(1)
	}
	if *output == "" {
		fmt.Fprintln(os.Stderr, "You must specify an output file with --output!")
		os.Exit(1)
	}

	var dlGroup sync.WaitGroup
	for i := 0; i < concurrentConnections; i++ {
		dlGroup.Add(1)
		go func() {
			MboxDownloader()
			dlGroup.Done()
		}()
	}

	go func() {
		dlGroup.Wait()
		close(msgCh)
	}()

	go func() {
		log.Printf("connecting to %s as user %s", *server, *username)
		c := Connect()
		ListMailboxes(c)
		Close(c)
		close(mboxCh)
	}()

	MsgWriter()
}
