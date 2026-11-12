package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/KalebHawkins/kryostate/internal/notify"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

const notifyUsage = `kryostate notify — where the daily digest of changes goes

Destinations are configured here and only here; the web interface shows
them and can send a test, but cannot add or change one. Webhook URLs and
SMTP passwords are given as credential references (env:NAME, file:/path,
exec:…, keyring:service/account), never as the value itself, so the
database never holds them. A reference is resolved by the process that
sends: the server for the daily digest, this command for -test.

  kryostate notify -list
  kryostate notify -add-webhook -name ops -url-ref env:KRYOSTATE_SLACK_URL [-format slack|generic]
  kryostate notify -add-email -name team -to ops@example.com,dba@example.com -from kryostate@example.com \
      -smtp smtp.example.com:587 -user kryostate -password-ref env:KRYOSTATE_SMTP_PASSWORD -starttls
  kryostate notify -remove ops
  kryostate notify -test ops
  kryostate notify -preview
  kryostate notify -base-url https://kryostate.example.com
  kryostate notify -at 08:00 | -enable | -disable

The slack format posts Slack's text and blocks, which a classic Teams
incoming webhook also accepts (it shows the text). The generic format posts
a JSON object with the digest's text and its contents, for a Teams workflow
or anything else that takes JSON. When and what is sent can also be changed
on the Notifications page.

Flags:
`

func cmdNotify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), notifyUsage)
		fs.PrintDefaults()
	}
	db := dbFlag(fs)
	o := notifyOpts{}
	fs.BoolVar(&o.list, "list", false, "list destinations, settings and recent sends")
	fs.BoolVar(&o.addWebhook, "add-webhook", false, "add a webhook destination")
	fs.BoolVar(&o.addEmail, "add-email", false, "add an email destination")
	fs.StringVar(&o.name, "name", "", "destination name")
	fs.StringVar(&o.urlRef, "url-ref", "", "credential reference that resolves to the webhook URL, such as env:KRYOSTATE_SLACK_URL")
	fs.StringVar(&o.format, "format", store.FormatSlack, "webhook body: slack or generic")
	fs.StringVar(&o.to, "to", "", "comma-separated recipients")
	fs.StringVar(&o.from, "from", "", "sender address")
	fs.StringVar(&o.smtp, "smtp", "", "SMTP server as host:port")
	fs.StringVar(&o.user, "user", "", "SMTP user name, if the server needs one")
	fs.StringVar(&o.passwordRef, "password-ref", "", "credential reference that resolves to the SMTP password")
	fs.BoolVar(&o.starttls, "starttls", false, "upgrade the SMTP connection with STARTTLS (usually port 587)")
	fs.BoolVar(&o.tls, "tls", false, "connect to the SMTP server over TLS (usually port 465)")
	fs.StringVar(&o.remove, "remove", "", "remove the named destination")
	fs.StringVar(&o.test, "test", "", "send a test message to the named destination")
	fs.BoolVar(&o.preview, "preview", false, "print the digest that would be sent now")
	fs.StringVar(&o.baseURL, "base-url", "", "address of this KryoState, for the link in each digest; \"-\" clears it")
	fs.StringVar(&o.at, "at", "", "local time of day to send the digest, HH:MM")
	fs.BoolVar(&o.enable, "enable", false, "turn the daily digest on")
	fs.BoolVar(&o.disable, "disable", false, "turn the daily digest off")
	fs.Parse(args)

	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	n := notify.New(st, secret.NewRegistry(nil), nil)
	return runNotify(ctx, n, o, os.Stdout)
}

type notifyOpts struct {
	list, addWebhook, addEmail, preview, starttls, tls, enable, disable bool
	name, urlRef, format, to, from, smtp, user, passwordRef             string
	remove, test, baseURL, at                                           string
}

func runNotify(ctx context.Context, n *notify.Notifier, o notifyOpts, w io.Writer) error {
	st := n.Store
	switch {
	case o.addWebhook && o.addEmail:
		return errors.New("use one of -add-webhook and -add-email")
	case o.addWebhook:
		if o.name == "" || o.urlRef == "" {
			return errors.New("-add-webhook needs -name and -url-ref")
		}
		if o.format != store.FormatSlack && o.format != store.FormatGeneric {
			return fmt.Errorf("-format is slack or generic, not %q", o.format)
		}
		if err := notify.CheckRef(n.Secrets, o.urlRef, "-url-ref"); err != nil {
			return err
		}
		if err := st.AddNotifyDest(ctx, store.NotifyDest{Name: o.name, Kind: store.DestWebhook,
			Format: o.format, URLRef: o.urlRef}, actor()); err != nil {
			return err
		}
		fmt.Fprintf(w, "added %s; try it with: kryostate notify -test %s\n", o.name, o.name)
		return nil
	case o.addEmail:
		return addEmail(ctx, n, o, w)
	case o.remove != "":
		if err := st.RemoveNotifyDest(ctx, o.remove, actor()); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s\n", o.remove)
		return nil
	case o.test != "":
		if err := n.SendTest(ctx, o.test, time.Now()); err != nil {
			return err
		}
		fmt.Fprintf(w, "test message sent to %s\n", o.test)
		return nil
	case o.preview:
		d, err := n.Preview(ctx, time.Now(), time.Local)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "Subject: "+d.Subject())
		fmt.Fprintln(w)
		fmt.Fprint(w, d.Text())
		if d.Quiet() {
			set, _ := st.GetNotifySettings(ctx)
			if !set.SendQuiet {
				fmt.Fprintln(w, "\n(nothing happened, so this would not be sent)")
			}
		}
		return nil
	case o.baseURL != "" || o.at != "" || o.enable || o.disable:
		return changeNotifySettings(ctx, st, o, w)
	default:
		return listNotify(ctx, st, w)
	}
}

func addEmail(ctx context.Context, n *notify.Notifier, o notifyOpts, w io.Writer) error {
	if o.name == "" || o.to == "" || o.from == "" || o.smtp == "" {
		return errors.New("-add-email needs -name, -to, -from and -smtp")
	}
	if o.starttls && o.tls {
		return errors.New("use one of -starttls and -tls")
	}
	if _, port, err := net.SplitHostPort(o.smtp); err != nil || port == "" {
		return fmt.Errorf("-smtp %q: want host:port, such as smtp.example.com:587", o.smtp)
	} else if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("-smtp %q: the port is not a number", o.smtp)
	}
	to := splitIDs(o.to)
	if err := notify.CheckAddresses(o.from, to); err != nil {
		return err
	}
	sec := store.SecurityNone
	switch {
	case o.starttls:
		sec = store.SecurityStartTLS
	case o.tls:
		sec = store.SecurityTLS
	}
	if o.passwordRef != "" {
		if o.user == "" {
			return errors.New("-password-ref goes with -user")
		}
		if err := notify.CheckRef(n.Secrets, o.passwordRef, "-password-ref"); err != nil {
			return err
		}
		if sec == store.SecurityNone {
			return errors.New("a password is only sent over an encrypted connection: add -starttls or -tls")
		}
	}
	if err := n.Store.AddNotifyDest(ctx, store.NotifyDest{Name: o.name, Kind: store.DestEmail, To: to,
		From: o.from, SMTPAddr: o.smtp, SMTPUser: o.user, PasswordRef: o.passwordRef, Security: sec}, actor()); err != nil {
		return err
	}
	fmt.Fprintf(w, "added %s; try it with: kryostate notify -test %s\n", o.name, o.name)
	return nil
}

func changeNotifySettings(ctx context.Context, st *store.Store, o notifyOpts, w io.Writer) error {
	if o.enable && o.disable {
		return errors.New("use one of -enable and -disable")
	}
	set, err := st.GetNotifySettings(ctx)
	if err != nil {
		return err
	}
	switch o.baseURL {
	case "":
	case "-":
		set.BaseURL = ""
	default:
		u, err := url.Parse(o.baseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("-base-url %q: want an http or https address, such as https://kryostate.example.com", o.baseURL)
		}
		set.BaseURL = strings.TrimRight(o.baseURL, "/")
	}
	if o.at != "" {
		h, m, err := notify.ParseClock(o.at)
		if err != nil {
			return err
		}
		set.At = fmt.Sprintf("%02d:%02d", h, m)
	}
	if o.enable {
		set.Enabled = true
	}
	if o.disable {
		set.Enabled = false
	}
	if err := st.PutNotifySettings(ctx, set, actor()); err != nil {
		return err
	}
	return listNotify(ctx, st, w)
}

func listNotify(ctx context.Context, st *store.Store, w io.Writer) error {
	set, err := st.GetNotifySettings(ctx)
	if err != nil {
		return err
	}
	dests, err := st.NotifyDests(ctx)
	if err != nil {
		return err
	}
	state := "off"
	if set.Enabled {
		state = "daily at " + set.At
	}
	fmt.Fprintf(w, "digest:   %s; sections %s", state, strings.Join(set.Sections, ", "))
	if set.SendQuiet {
		fmt.Fprint(w, "; sent even when nothing happened")
	}
	fmt.Fprintln(w)
	base := set.BaseURL
	if base == "" {
		base = "(none; set with -base-url)"
	}
	fmt.Fprintf(w, "link:     %s\n\n", base)
	if len(dests) == 0 {
		fmt.Fprintln(w, "No destinations. Add one with -add-webhook or -add-email.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tWHERE\tSECRET")
	for _, d := range dests {
		switch d.Kind {
		case store.DestWebhook:
			fmt.Fprintf(tw, "%s\t%s webhook\t-\t%s\n", d.Name, d.Format, d.URLRef)
		default:
			pw := "-"
			if d.PasswordRef != "" {
				pw = d.PasswordRef
			}
			fmt.Fprintf(tw, "%s\temail\t%s via %s (%s)\t%s\n", d.Name, strings.Join(d.To, ","), d.SMTPAddr, d.Security, pw)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	sends, err := st.NotifySends(ctx, 5)
	if err != nil || len(sends) == 0 {
		return err
	}
	fmt.Fprintln(w, "\nrecent sends:")
	for _, s := range sends {
		res := "ok"
		switch {
		case s.Kind == store.SendSkipped:
			res = "nothing happened, not sent"
		case !s.OK:
			res = "failed: " + s.Error
		}
		fmt.Fprintf(w, "  %s  %-7s %-10s %s\n", s.At.Local().Format("2006-01-02 15:04"), s.Kind, s.Dest, res)
	}
	return nil
}
