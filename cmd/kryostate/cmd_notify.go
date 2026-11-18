package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/KalebHawkins/kryostate/internal/notify"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

const notifyLong = `Where the daily digest of changes goes.

Destinations are configured here and only here; the web interface shows
them and can send a test, but cannot add or change one. Webhook URLs and
SMTP passwords are given as credential references (env:NAME, file:/path,
exec:…, keyring:service/account), never as the value itself, so the
database never holds them. A reference is resolved by the process that
sends: the server for the daily digest, this command for "notify test".

The slack format posts Slack's text and blocks, which a classic Teams
incoming webhook also accepts (it shows the text). The generic format posts
a JSON object with the digest's text and its contents, for a Teams workflow
or anything else that takes JSON. When and what is sent can also be changed
on the Notifications page.`

const notifyExample = `  kryostate notify list
  kryostate notify add-webhook ops --url-ref env:KRYOSTATE_SLACK_URL [--format slack|generic]
  kryostate notify add-email team --to ops@example.com,dba@example.com --from kryostate@example.com \
      --smtp smtp.example.com:587 --user kryostate --password-ref env:KRYOSTATE_SMTP_PASSWORD --starttls
  kryostate notify rm ops
  kryostate notify test ops
  kryostate notify preview
  kryostate notify set --base-url https://kryostate.example.com
  kryostate notify set --at 08:00 --enable`

func (a *app) notifyCmd() *cobra.Command {
	cmd := group("notify", "send a daily digest of changes to chat or email")
	cmd.Long, cmd.Example = notifyLong, notifyExample
	run := func(o *notifyOpts, set func(args []string)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			set(args)
			st, err := openStore(a.db)
			if err != nil {
				return err
			}
			defer st.Close()
			return runNotify(cmd.Context(), notify.New(st, secret.NewRegistry(nil), nil), *o, os.Stdout)
		}
	}
	none := func([]string) {}

	var lo notifyOpts
	list := &cobra.Command{
		Use: "list", Aliases: []string{"ls"}, Args: cobra.NoArgs,
		Short: "list destinations, settings and recent sends",
		RunE:  run(&lo, func([]string) { lo.list = true }),
	}

	var wo notifyOpts
	webhook := &cobra.Command{
		Use: "add-webhook <name>", Args: cobra.ExactArgs(1),
		Short: "add a webhook destination",
		RunE:  run(&wo, func(args []string) { wo.addWebhook, wo.name = true, args[0] }),
	}
	webhook.Flags().StringVar(&wo.urlRef, "url-ref", "", "credential reference that resolves to the webhook URL, such as env:KRYOSTATE_SLACK_URL (required)")
	webhook.Flags().StringVar(&wo.format, "format", store.FormatSlack, "webhook body: slack or generic")
	_ = webhook.MarkFlagRequired("url-ref")

	var eo notifyOpts
	email := &cobra.Command{
		Use: "add-email <name>", Args: cobra.ExactArgs(1),
		Short: "add an email destination",
		RunE:  run(&eo, func(args []string) { eo.addEmail, eo.name = true, args[0] }),
	}
	ef := email.Flags()
	ef.StringVar(&eo.to, "to", "", "comma-separated recipients")
	ef.StringVar(&eo.from, "from", "", "sender address")
	ef.StringVar(&eo.smtp, "smtp", "", "SMTP server as host:port")
	ef.StringVar(&eo.user, "user", "", "SMTP user name, if the server needs one")
	ef.StringVar(&eo.passwordRef, "password-ref", "", "credential reference that resolves to the SMTP password")
	ef.BoolVar(&eo.starttls, "starttls", false, "upgrade the SMTP connection with STARTTLS (usually port 587)")
	ef.BoolVar(&eo.tls, "tls", false, "connect to the SMTP server over TLS (usually port 465)")

	var ro notifyOpts
	rm := &cobra.Command{
		Use: "rm <name>", Aliases: []string{"remove"}, Args: cobra.ExactArgs(1),
		Short: "remove a destination",
		RunE:  run(&ro, func(args []string) { ro.remove = args[0] }),
	}

	var to notifyOpts
	test := &cobra.Command{
		Use: "test <name>", Args: cobra.ExactArgs(1),
		Short: "send a test message to a destination",
		RunE:  run(&to, func(args []string) { to.test = args[0] }),
	}

	var po notifyOpts
	preview := &cobra.Command{
		Use: "preview", Args: cobra.NoArgs,
		Short: "print the digest that would be sent now",
		RunE:  run(&po, func([]string) { po.preview = true }),
	}

	var so notifyOpts
	set := &cobra.Command{
		Use: "set", Args: cobra.NoArgs,
		Short:   "change when the digest is sent, whether it is, and the link in it",
		Example: "  kryostate notify set --at 07:30 --enable\n  kryostate notify set --base-url https://kryostate.example.com",
		RunE:    run(&so, none),
	}
	sf := set.Flags()
	sf.StringVar(&so.baseURL, "base-url", "", "address of this KryoState, for the link in each digest; \"-\" clears it")
	sf.StringVar(&so.at, "at", "", "local time of day to send the digest, HH:MM")
	sf.BoolVar(&so.enable, "enable", false, "turn the daily digest on")
	sf.BoolVar(&so.disable, "disable", false, "turn the daily digest off")
	set.MarkFlagsOneRequired("base-url", "at", "enable", "disable")

	cmd.AddCommand(list, webhook, email, rm, test, preview, set)
	return cmd
}
