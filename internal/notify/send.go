package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// Network limits. A digest is small; anything slower than this is broken.
const (
	HTTPTimeout = 20 * time.Second
	SMTPTimeout = 30 * time.Second
	userAgent   = "KryoState"
)

// newHTTPClient returns the client webhooks are posted with. It follows a
// redirect only to the same host over the same or a stronger scheme: a
// webhook that answers with a redirect elsewhere is either misconfigured or
// trying to make this server talk to somewhere it was not told to.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: HTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			first := via[0].URL
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if !strings.EqualFold(req.URL.Host, first.Host) {
				return errors.New("refusing to follow a redirect to another host")
			}
			if first.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("refusing to follow a redirect from https to http")
			}
			return nil
		},
	}
}

// checkWebhookURL accepts only an absolute http or https URL.
func checkWebhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("the webhook reference does not resolve to a URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("the webhook reference must resolve to an http or https URL")
	}
	if u.User != nil {
		return nil, errors.New("the webhook URL must not carry a user name or password")
	}
	return u, nil
}

// postWebhook sends one message. Errors never include the URL: for most
// webhooks the URL is the credential.
func postWebhook(ctx context.Context, client *http.Client, rawURL, format string, m Message) error {
	if _, err := checkWebhookURL(rawURL); err != nil {
		return err
	}
	var payload any
	switch format {
	case store.FormatGeneric:
		payload = m.Generic
	default:
		payload = m.SlackBody()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not build the webhook request")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// Dialer opens a connection, so tests can stand in for the network.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// CheckAddresses parses a sender and recipients, refusing anything that
// would not survive being put in a header.
func CheckAddresses(from string, to []string) error {
	if _, err := mail.ParseAddress(from); err != nil {
		return fmt.Errorf("from address %q: %v", from, err)
	}
	if len(to) == 0 {
		return errors.New("an email destination needs at least one recipient")
	}
	for _, a := range to {
		if _, err := mail.ParseAddress(a); err != nil {
			return fmt.Errorf("recipient %q: %v", a, err)
		}
	}
	return nil
}

// BuildEmail renders a message as an RFC 5322 email with a quoted-printable
// plain text body.
func BuildEmail(from string, to []string, m Message, now time.Time) ([]byte, error) {
	if err := CheckAddresses(from, to); err != nil {
		return nil, err
	}
	fa, _ := mail.ParseAddress(from)
	var tos []string
	for _, a := range to {
		pa, _ := mail.ParseAddress(a)
		tos = append(tos, pa.String())
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	host := "kryostate.invalid"
	if _, d, ok := strings.Cut(fa.Address, "@"); ok && d != "" {
		host = d
	}
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", fa.String())
	hdr("To", strings.Join(tos, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject)))
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+host+">")
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	hdr("Auto-Submitted", "auto-generated")
	hdr("User-Agent", userAgent)
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	if _, err := qp.Write([]byte(text)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// sendMail delivers one email. With STARTTLS the server must offer it: a
// server that does not is refused rather than sent the password in clear.
func sendMail(ctx context.Context, dial Dialer, tlsConf *tls.Config, d store.NotifyDest, password string, msg []byte) error {
	host, _, err := net.SplitHostPort(d.SMTPAddr)
	if err != nil {
		return fmt.Errorf("smtp address %q: want host:port", d.SMTPAddr)
	}
	ctx, cancel := context.WithTimeout(ctx, SMTPTimeout)
	defer cancel()
	if dial == nil {
		var nd net.Dialer
		dial = nd.DialContext
	}
	conn, err := dial(ctx, "tcp", d.SMTPAddr)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	// Closing the connection is what interrupts a stalled exchange when the
	// context ends early.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if tlsConf != nil {
		cfg = tlsConf.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
	}
	if d.Security == store.SecurityTLS {
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fmt.Errorf("smtp tls: %w", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if err := c.Hello("kryostate"); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	if d.Security == store.SecurityStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: the server does not offer STARTTLS; refusing to continue unencrypted")
		}
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}
	if d.SMTPUser != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: the server does not offer authentication")
		}
		// PlainAuth itself refuses to send a password over a connection
		// that is not encrypted, unless the server is this machine.
		if err := c.Auth(smtp.PlainAuth("", d.SMTPUser, password, host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	fa, _ := mail.ParseAddress(d.From)
	if err := c.Mail(fa.Address); err != nil {
		return fmt.Errorf("smtp from: %w", err)
	}
	for _, a := range d.To {
		pa, _ := mail.ParseAddress(a)
		if err := c.Rcpt(pa.Address); err != nil {
			return fmt.Errorf("smtp recipient %s: %w", pa.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return c.Quit()
}
