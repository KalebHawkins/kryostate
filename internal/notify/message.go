package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// Message is something to deliver, already rendered for every kind of
// destination.
type Message struct {
	Subject string
	// Text is the plain text body.
	Text string
	// Sections are Slack mrkdwn blocks, one per digest section.
	Sections []string
	// Generic is the structured body a generic webhook receives.
	Generic map[string]any
}

// Slack limits a section block's text to 3000 characters and a message to
// 50 blocks.
const (
	slackSectionMax = 2900
	slackBlocksMax  = 48
)

// Message renders the digest for every destination kind.
func (d Digest) Message() Message {
	m := Message{Subject: d.Subject(), Text: d.Text()}
	m.Sections = d.slackSections()
	m.Generic = d.generic()
	return m
}

// TestMessage is what "send a test" delivers.
func TestMessage(now time.Time) Message {
	text := "KryoState test message, sent " + now.Format("2006-01-02 15:04 MST") +
		". If you can read this, the daily digest can reach this destination."
	return Message{
		Subject:  "KryoState test message",
		Text:     text + "\n",
		Sections: []string{slackEscape(text)},
		Generic:  map[string]any{"source": "KryoState", "test": true, "subject": "KryoState test message", "text": text},
	}
}

// slackEscape escapes the three characters Slack's mrkdwn treats as markup.
// Values come from remote hosts and must not become links or mentions.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func slackLink(url, label string) string {
	if url == "" {
		return slackEscape(label)
	}
	// A label cannot contain the delimiters, and the URL is built here from
	// the base URL and an id.
	return "<" + strings.NewReplacer("|", "%7C", ">", "%3E", "<", "%3C").Replace(url) + "|" + slackEscape(label) + ">"
}

func (d Digest) slackSections() []string {
	var out []string
	head := "*KryoState daily digest*\n" + slackEscape(d.Window())
	if l := d.Link(); l != "" {
		head += "\n" + slackLink(l, "Open the Changes page")
	}
	out = append(out, head)
	if d.Settings.Has(store.SectionChanges) {
		var b strings.Builder
		if d.ChangeCount == 0 {
			b.WriteString("*No values changed.*")
		} else {
			fmt.Fprintf(&b, "*%s on %s%s*", plural(d.ChangeCount, "value changed", "values changed"),
				plural(d.HostsChanged, "host", "hosts"), moreSuffix(d.ChangesLimited))
		}
		out = append(out, b.String())
		for _, h := range d.Changes {
			var hb strings.Builder
			hb.WriteString("*" + slackLink(d.HostLink(h.CIID), h.Host) + "*")
			for _, c := range h.Items {
				fmt.Fprintf(&hb, "\n• `%s`: %s → %s", slackEscape(attrName(c)), slackEscape(short(c.Old)), slackEscape(short(c.New)))
			}
			if h.More > 0 {
				fmt.Fprintf(&hb, "\n… and %d more on this host", h.More)
			}
			out = append(out, hb.String())
		}
		if d.MoreHosts > 0 {
			out = append(out, fmt.Sprintf("… and %d more %s", d.MoreHosts, hostWord(d.MoreHosts)))
		}
	}
	if d.Settings.Has(store.SectionHealth) && (len(d.StartedFailing) > 0 || len(d.Recovered) > 0) {
		var b strings.Builder
		if len(d.StartedFailing) > 0 {
			fmt.Fprintf(&b, "*%s started failing*", plural(len(d.StartedFailing), "host", "hosts"))
			for i, h := range d.StartedFailing {
				if i == MaxHealthHosts {
					fmt.Fprintf(&b, "\n… and %d more", len(d.StartedFailing)-MaxHealthHosts)
					break
				}
				fmt.Fprintf(&b, "\n• %s: %s", slackLink(d.HostLink(h.CIID), h.Label), slackEscape(healthDetail(h)))
			}
		}
		if len(d.Recovered) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "*%s recovered*", plural(len(d.Recovered), "host", "hosts"))
			for i, h := range d.Recovered {
				if i == MaxHealthHosts {
					fmt.Fprintf(&b, "\n… and %d more", len(d.Recovered)-MaxHealthHosts)
					break
				}
				fmt.Fprintf(&b, "\n• %s", slackLink(d.HostLink(h.CIID), h.Label))
			}
		}
		out = append(out, b.String())
	}
	if d.Settings.Has(store.SectionNever) && d.NeverCount > 0 {
		out = append(out, fmt.Sprintf("*%s never collected*\n%s", plural(d.NeverCount, "host", "hosts"), slackEscape(neverList(d))))
	}
	for _, x := range d.Extra {
		var b strings.Builder
		b.WriteString("*" + slackEscape(x.Title) + "*")
		for _, l := range x.Lines {
			b.WriteString("\n• " + slackEscape(l))
		}
		out = append(out, b.String())
	}
	for i, s := range out {
		if len(s) > slackSectionMax {
			out[i] = cutUTF8(s, slackSectionMax) + "…"
		}
	}
	if len(out) > slackBlocksMax {
		out = append(out[:slackBlocksMax-1], "… more in KryoState")
	}
	return out
}

// cutUTF8 cuts s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// SlackBody is the JSON a Slack-compatible webhook receives. text is the
// fallback notifications and older receivers (such as a Teams connector)
// show; blocks is the formatted message.
func (m Message) SlackBody() map[string]any {
	blocks := make([]map[string]any, 0, len(m.Sections))
	for _, s := range m.Sections {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": s},
		})
	}
	text := m.Text
	if len(text) > 3500 {
		text = cutUTF8(text, 3500) + "…"
	}
	return map[string]any{"text": text, "blocks": blocks}
}

func (d Digest) generic() map[string]any {
	type change struct {
		Attr  string    `json:"attr"`
		Scope string    `json:"scope,omitempty"`
		Old   string    `json:"old"`
		New   string    `json:"new"`
		At    time.Time `json:"at"`
	}
	type host struct {
		ID      string   `json:"id"`
		Host    string   `json:"host"`
		URL     string   `json:"url,omitempty"`
		Changes []change `json:"changes"`
		More    int      `json:"more,omitempty"`
	}
	type health struct {
		ID      string    `json:"id"`
		Host    string    `json:"host"`
		Outcome string    `json:"outcome"`
		Detail  string    `json:"detail,omitempty"`
		At      time.Time `json:"at"`
	}
	hosts := []host{}
	for _, h := range d.Changes {
		x := host{ID: h.CIID, Host: h.Host, URL: d.HostLink(h.CIID), More: h.More, Changes: []change{}}
		for _, c := range h.Items {
			x.Changes = append(x.Changes, change{Attr: c.Attr, Scope: c.Scope, Old: c.Old, New: c.New, At: c.At.UTC()})
		}
		hosts = append(hosts, x)
	}
	conv := func(fs []store.FailingHost) []health {
		out := []health{}
		for _, f := range fs {
			out = append(out, health{ID: f.CIID, Host: f.Label, Outcome: f.Outcome, Detail: f.Detail, At: f.At.UTC()})
		}
		return out
	}
	never := []string{}
	for _, h := range d.Never {
		never = append(never, h.Label)
	}
	extra := []map[string]any{}
	for _, x := range d.Extra {
		extra = append(extra, map[string]any{"key": x.Key, "title": x.Title, "count": x.Count, "lines": x.Lines})
	}
	body := map[string]any{
		"source":  "KryoState",
		"subject": d.Subject(),
		"text":    d.Text(),
		"since":   d.Since.UTC(),
		"until":   d.Until.UTC(),
		"counts": map[string]int{
			"changes": d.ChangeCount, "hosts_changed": d.HostsChanged,
			"started_failing": len(d.StartedFailing), "recovered": len(d.Recovered), "never_collected": d.NeverCount,
		},
		"changes":         hosts,
		"started_failing": conv(d.StartedFailing),
		"recovered":       conv(d.Recovered),
		"never_collected": never,
		"sections":        extra,
	}
	if l := d.Link(); l != "" {
		body["url"] = l
	}
	return body
}
