// PROTOTYPE, throwaway: can whatsmeow link as a WhatsApp device from a gVisor
// container whose only route out is a relay to web.whatsapp.com, and does the
// session survive a restart with its store on a volume?
//
//	wa /data/wa.db              # link by QR (prints the codes), or resume
//	wa /data/wa.db +15551234567 # link by phone code instead of a QR
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"modernc.org/sqlite"
)

func init() { sql.Register("sqlite3", &sqlite.Driver{}) }

func logf(f string, a ...any) {
	fmt.Printf("%s "+f+"\n", append([]any{time.Now().UTC().Format("15:04:05")}, a...)...)
}

func main() {
	ctx := context.Background()
	db, err := sqlstore.New(ctx, "sqlite3", "file:"+os.Args[1]+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", waLog.Stdout("db", "WARN", false))
	if err != nil {
		logf("store: %v", err)
		os.Exit(1)
	}
	dev, err := db.GetFirstDevice(ctx)
	if err != nil {
		logf("device: %v", err)
		os.Exit(1)
	}
	cli := whatsmeow.NewClient(dev, waLog.Stdout("wa", "INFO", false))
	cli.AddEventHandler(func(e any) {
		switch v := e.(type) {
		case *events.Message:
			logf("message from %s (chat %s): %q", v.Info.Sender, v.Info.Chat, v.Message.GetConversation())
		case *events.Connected:
			logf("connected as %v", cli.Store.ID)
		case *events.Disconnected:
			logf("disconnected")
		case *events.LoggedOut:
			logf("logged out: %v", v.Reason)
		case *events.PairSuccess:
			logf("paired: %v", v.ID)
		}
	})
	if cli.Store.ID == nil {
		logf("no session in the store: linking")
		qr, _ := cli.GetQRChannel(ctx)
		if err := cli.Connect(); err != nil {
			logf("connect: %v", err)
			os.Exit(1)
		}
		if len(os.Args) > 2 {
			code, err := cli.PairPhone(ctx, os.Args[2], true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			logf("phone link code: %s err=%v", code, err)
		}
		for item := range qr {
			if item.Event == "code" {
				logf("QR code (%d chars, valid %s): %s", len(item.Code), item.Timeout, item.Code)
			} else {
				logf("link event: %s %v", item.Event, item.Error)
			}
		}
	} else {
		logf("session found in the store: %v", cli.Store.ID)
		if err := cli.Connect(); err != nil {
			logf("connect: %v", err)
			os.Exit(1)
		}
	}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c
	cli.Disconnect()
}
