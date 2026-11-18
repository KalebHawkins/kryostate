package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/notify"
	"github.com/KalebHawkins/kryostate/internal/store"
)

type notifyOpts struct {
	list, addWebhook, addEmail, preview, starttls, tls, enable, disable bool
	name, urlRef, format, to, from, smtp, user, passwordRef             string
	remove, test, baseURL, at                                           string
}

func runNotify(ctx context.Context, n *notify.Notifier, o notifyOpts, w io.Writer) error {
	st := n.Store
	switch {
	case o.addWebhook && o.addEmail:
		return errors.New("use one of add-webhook and add-email")
	case o.addWebhook:
		if o.name == "" || o.urlRef == "" {
			return errors.New("add-webhook needs a name and --url-ref")
		}
		if o.format != store.FormatSlack && o.format != store.FormatGeneric {
			return fmt.Errorf("--format is slack or generic, not %q", o.format)
		}
		if err := notify.CheckRef(n.Secrets, o.urlRef, "--url-ref"); err != nil {
			return err
		}
		if err := st.AddNotifyDest(ctx, store.NotifyDest{Name: o.name, Kind: store.DestWebhook,
			Format: o.format, URLRef: o.urlRef}, actor()); err != nil {
			return err
		}
		fmt.Fprintf(w, "added %s; try it with: kryostate notify test %s\n", o.name, o.name)
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
		return errors.New("add-email needs a name, --to, --from and --smtp")
	}
	if o.starttls && o.tls {
		return errors.New("use one of --starttls and --tls")
	}
	if _, port, err := net.SplitHostPort(o.smtp); err != nil || port == "" {
		return fmt.Errorf("--smtp %q: want host:port, such as smtp.example.com:587", o.smtp)
	} else if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("--smtp %q: the port is not a number", o.smtp)
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
			return errors.New("--password-ref goes with --user")
		}
		if err := notify.CheckRef(n.Secrets, o.passwordRef, "--password-ref"); err != nil {
			return err
		}
		if sec == store.SecurityNone {
			return errors.New("a password is only sent over an encrypted connection: add --starttls or --tls")
		}
	}
	if err := n.Store.AddNotifyDest(ctx, store.NotifyDest{Name: o.name, Kind: store.DestEmail, To: to,
		From: o.from, SMTPAddr: o.smtp, SMTPUser: o.user, PasswordRef: o.passwordRef, Security: sec}, actor()); err != nil {
		return err
	}
	fmt.Fprintf(w, "added %s; try it with: kryostate notify test %s\n", o.name, o.name)
	return nil
}

func changeNotifySettings(ctx context.Context, st *store.Store, o notifyOpts, w io.Writer) error {
	if o.enable && o.disable {
		return errors.New("use one of --enable and --disable")
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
			return fmt.Errorf("--base-url %q: want an http or https address, such as https://kryostate.example.com", o.baseURL)
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
		base = "(none; set with: kryostate notify set --base-url <url>)"
	}
	fmt.Fprintf(w, "link:     %s\n\n", base)
	if len(dests) == 0 {
		fmt.Fprintln(w, "No destinations. Add one with: kryostate notify add-webhook or add-email.")
		return nil
	}
	tw := newTable(w)
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
