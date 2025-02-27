// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package task

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"text/template"
	"time"

	sendgrid "github.com/sendgrid/sendgrid-go"
	sendgridmail "github.com/sendgrid/sendgrid-go/helpers/mail"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	goldmarktext "github.com/yuin/goldmark/text"
	"golang.org/x/build/internal/workflow"
	"golang.org/x/build/maintner/maintnerd/maintapi/version"
	"golang.org/x/net/html"
)

type ReleaseAnnouncement struct {
	// Version is the Go version that has been released.
	//
	// The version string must use the same format as Go tags. For example:
	// 	• "go1.17.2" for a minor Go release
	// 	• "go1.18" for a major Go release
	// 	• "go1.18beta1" or "go1.18rc1" for a pre-release
	Version string
	// SecondaryVersion is an older Go version that was also released.
	// This only applies to minor releases. For example, "go1.16.10".
	SecondaryVersion string

	// Security is a list of descriptions, one for each distinct
	// security fix included in this release, in Markdown format.
	//
	// The empty list means there are no security fixes included.
	//
	// This field applies only to minor releases; it is an error
	// to try to use it another release type.
	Security []string

	// Names is an optional list of release coordinator names to
	// include in the sign-off message.
	Names []string
}

// AnnounceMailTasks contains tasks related to the release announcement email.
type AnnounceMailTasks struct {
	SendGridAPIKey string

	From mail.Address // An RFC 5322 address. For example, "Barry Gibbs <bg@example.com>".
	To   mail.Address
	BCC  []mail.Address
}

// SentMail represents an email that was sent.
type SentMail struct {
	Subject string // Subject of the email. Expected to be unique so it can be used to identify the email.
}

// AnnounceMinorRelease sends an email announcing a minor Go release to Google Groups.
func (t AnnounceMailTasks) AnnounceMinorRelease(ctx *workflow.TaskContext, r ReleaseAnnouncement) (SentMail, error) {
	if err := verifyGoVersions(r.Version, r.SecondaryVersion); err != nil {
		return SentMail{}, err
	}

	return t.announceRelease(ctx, r)
}

// AnnounceBetaRelease sends an email announcing a beta Go release to Google Groups.
func (t AnnounceMailTasks) AnnounceBetaRelease(ctx *workflow.TaskContext, r ReleaseAnnouncement) (SentMail, error) {
	if r.SecondaryVersion != "" {
		return SentMail{}, fmt.Errorf("got 2 Go versions, want 1")
	}
	if err := verifyGoVersions(r.Version); err != nil {
		return SentMail{}, err
	}

	return t.announceRelease(ctx, r)
}

// AnnounceRCRelease sends an email announcing a Go release candidate to Google Groups.
func (t AnnounceMailTasks) AnnounceRCRelease(ctx *workflow.TaskContext, r ReleaseAnnouncement) (SentMail, error) {
	if r.SecondaryVersion != "" {
		return SentMail{}, fmt.Errorf("got 2 Go versions, want 1")
	}
	if err := verifyGoVersions(r.Version); err != nil {
		return SentMail{}, err
	}

	return t.announceRelease(ctx, r)
}

// AnnounceMajorRelease sends an email announcing a major Go release to Google Groups.
func (t AnnounceMailTasks) AnnounceMajorRelease(ctx *workflow.TaskContext, r ReleaseAnnouncement) (SentMail, error) {
	if r.SecondaryVersion != "" {
		return SentMail{}, fmt.Errorf("got 2 Go versions, want 1")
	}
	if err := verifyGoVersions(r.Version); err != nil {
		return SentMail{}, err
	}

	return t.announceRelease(ctx, r)
}

// announceRelease sends an email announcing a Go release.
func (t AnnounceMailTasks) announceRelease(ctx *workflow.TaskContext, r ReleaseAnnouncement) (SentMail, error) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < time.Minute {
		return SentMail{}, fmt.Errorf("insufficient time for announce release task; a minimum of a minute left on context is required")
	}

	// Generate the announcement email.
	m, err := announcementMail(r)
	if err != nil {
		return SentMail{}, err
	}
	if log := ctx.Logger; log != nil {
		log.Printf("announcement subject: %s\n", m.Subject)
		log.Printf("\nannouncement body HTML:\n%s", m.BodyHTML)
		log.Printf("\nannouncement body text:\n%s", m.BodyText)
	}

	// Confirm that this announcement doesn't already exist.
	if threadURL, err := findGoogleGroupsThread(ctx, m.Subject); err != nil {
		// Proceeding would risk sending a duplicate email, so error out instead.
		return SentMail{}, fmt.Errorf("stopping early due to error checking for an existing Google Groups thread: %v", err)
	} else if threadURL != "" {
		// TODO(go.dev/issue/47406): Once this task is a part of a larger workflow (which may need
		// to tolerate resuming, restarting, and so on), the case of the matching subject already
		// being there should become considered as "success, keep going" rather than "error, stop".
		return SentMail{}, fmt.Errorf("a Google Groups thread with matching subject %q already exists at %q, stopping", m.Subject, threadURL)
	}

	// Send the announcement email to the destination mailing lists.
	if t.SendGridAPIKey == "" {
		return SentMail{Subject: "[dry-run] " + m.Subject}, nil
	}
	err = t.sendMailViaSendGrid(m)
	if err != nil {
		return SentMail{}, err
	}

	return SentMail{m.Subject}, nil
}

type mailContent struct {
	Subject  string
	BodyHTML string
	BodyText string
}

// announcementMail generates the announcement email for release r.
func announcementMail(r ReleaseAnnouncement) (mailContent, error) {
	// Pick a template name for this type of release.
	var name string
	if i := strings.Index(r.Version, "beta"); i != -1 { // A beta release.
		name = "announce-beta.md"
	} else if i := strings.Index(r.Version, "rc"); i != -1 { // Release Candidate.
		name = "announce-rc.md"
	} else if strings.Count(r.Version, ".") == 1 { // Major release like "go1.X".
		name = "announce-major.md"
	} else if strings.Count(r.Version, ".") == 2 { // Minor release like "go1.X.Y".
		name = "announce-minor.md"
	} else {
		return mailContent{}, fmt.Errorf("unknown version format: %q", r.Version)
	}

	if len(r.Security) > 0 && name != "announce-minor.md" {
		// The Security field isn't supported in templates other than minor,
		// so report an error instead of silently dropping it.
		//
		// Note: Maybe in the future we'd want to consider support for including sentences like
		// "This beta release includes the same security fixes as in Go X.Y.Z and Go A.B.C.",
		// but we'll have a better idea after these initial templates get more practical use.
		return mailContent{}, fmt.Errorf("email template %q doesn't support the Security field; this field can only be used in minor releases", name)
	}

	// Render the announcement email template.
	//
	// It'll produce a valid message with a MIME header and a body, so parse it as such.
	var buf bytes.Buffer
	if err := announceTmpl.ExecuteTemplate(&buf, name, r); err != nil {
		return mailContent{}, err
	}
	m, err := mail.ReadMessage(&buf)
	if err != nil {
		return mailContent{}, fmt.Errorf(`email template must be formatted like a mail message, but reading it failed: %v`, err)
	}

	// Get the email subject (it's a plain string, no further processing needed).
	if _, ok := m.Header["Subject"]; !ok {
		return mailContent{}, fmt.Errorf(`email template must have a "Subject" key in its MIME header, but it's not found`)
	} else if n := len(m.Header["Subject"]); n != 1 {
		return mailContent{}, fmt.Errorf(`email template must have a single "Subject" value in its MIME header, but have %d values`, n)
	}
	subject := m.Header.Get("Subject")

	// Render the email body, in Markdown format at this point, to HTML and plain text.
	html, text, err := renderMarkdown(m.Body)
	if err != nil {
		return mailContent{}, err
	}

	return mailContent{subject, html, text}, nil
}

// announceTmpl holds templates for Go release announcement emails.
//
// Each email template starts with a MIME-style header with a Subject key,
// and the rest of it is Markdown for the email body.
var announceTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"join": func(s []string) string {
		switch len(s) {
		case 0:
			return ""
		case 1:
			return s[0]
		case 2:
			return s[0] + " and " + s[1]
		default:
			return strings.Join(s[:len(s)-1], ", ") + ", and " + s[len(s)-1]
		}
	},
	"indent": func(s string) string { return "\t" + strings.ReplaceAll(s, "\n", "\n\t") },

	// subjectPrefix returns the email subject prefix for release r, if any.
	"subjectPrefix": func(r ReleaseAnnouncement) string {
		switch {
		case len(r.Security) > 0:
			// Include a security prefix as documented at https://go.dev/security#receiving-security-updates:
			//
			//	> The best way to receive security announcements is to subscribe to the golang-announce mailing list.
			//	> Any messages pertaining to a security issue will be prefixed with [security].
			//
			return "[security]"
		default:
			return ""
		}
	},

	// short and helpers below manipulate valid Go version strings
	// for the current needs of the announcement templates.
	"short": func(v string) string { return strings.TrimPrefix(v, "go") },
	// major extracts the major part of a valid Go version.
	// For example, major("go1.18.4") == "1.18".
	"major": func(v string) (string, error) {
		x, ok := version.Go1PointX(v)
		if !ok {
			return "", fmt.Errorf("internal error: version.Go1PointX(%q) is not ok", v)
		}
		return fmt.Sprintf("1.%d", x), nil
	},
	// build extracts the pre-release build number of a valid Go version.
	// For example, build("go1.19beta2") == "2".
	"build": func(v string) (string, error) {
		if i := strings.Index(v, "beta"); i != -1 {
			return v[i+len("beta"):], nil
		} else if i := strings.Index(v, "rc"); i != -1 {
			return v[i+len("rc"):], nil
		}
		return "", fmt.Errorf("internal error: unhandled pre-release Go version %q", v)
	},
}).ParseFS(tmplDir, "template/announce-*.md"))

//go:embed template
var tmplDir embed.FS

// sendMailViaSendGrid sends an email by making
// an authenticated request to the SendGrid API.
func (t AnnounceMailTasks) sendMailViaSendGrid(m mailContent) error {
	from, to := sendgridmail.Email(t.From), sendgridmail.Email(t.To)
	req := sendgridmail.NewSingleEmail(&from, m.Subject, &to, m.BodyText, m.BodyHTML)
	if len(req.Personalizations) != 1 {
		return fmt.Errorf("internal error: len(req.Personalizations) is %d, want 1", len(req.Personalizations))
	}
	for _, bcc := range t.BCC {
		bcc := sendgridmail.Email(bcc)
		req.Personalizations[0].AddBCCs(&bcc)
	}

	no := false
	req.TrackingSettings = &sendgridmail.TrackingSettings{
		ClickTracking:        &sendgridmail.ClickTrackingSetting{Enable: &no},
		OpenTracking:         &sendgridmail.OpenTrackingSetting{Enable: &no},
		SubscriptionTracking: &sendgridmail.SubscriptionTrackingSetting{Enable: &no},
	}

	sg := sendgrid.NewSendClient(t.SendGridAPIKey)
	resp, err := sg.Send(req)
	if err != nil {
		return err
	} else if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("unexpected status %d %s, want 202 Accepted; body = %s", resp.StatusCode, http.StatusText(resp.StatusCode), resp.Body)
	}
	return nil
}

// AwaitAnnounceMail waits for an announcement email with the specified subject
// to show up on Google Groups, and returns its canonical URL.
func (t AnnounceMailTasks) AwaitAnnounceMail(ctx *workflow.TaskContext, m SentMail) (announcementURL string, _ error) {
	// Find the URL for the announcement while giving the email a chance to be received and moderated.
	started := time.Now()
	poll := time.NewTicker(10 * time.Second)
	defer poll.Stop()
	updateLog := time.NewTicker(time.Minute)
	defer updateLog.Stop()
	for {
		// Wait a bit, updating the log periodically.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-poll.C:
		case t := <-updateLog.C:
			if log := ctx.Logger; log != nil {
				log.Printf("... still waiting for %q to appear after %v ...\n", m.Subject, t.Sub(started))
			}
			continue
		}

		// See if our email is available by now.
		threadURL, err := findGoogleGroupsThread(ctx, m.Subject)
		if err != nil {
			if log := ctx.Logger; log != nil {
				log.Printf("findGoogleGroupsThread: %v", err)
			}
			continue
		} else if threadURL == "" {
			// Our email hasn't yet shown up. Wait more and try again.
			continue
		}
		return threadURL, nil
	}
}

// findGoogleGroupsThread fetches the first page of threads from the golang-announce
// Google Groups mailing list, and looks for a thread with the matching subject line.
// It returns its URL if found or the empty string if not found.
func findGoogleGroupsThread(ctx *workflow.TaskContext, subject string) (threadURL string, _ error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://groups.google.com/g/golang-announce", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("did not get acceptable status code: %v body: %q", resp.Status, body)
	}
	if ct, want := resp.Header.Get("Content-Type"), "text/html; charset=utf-8"; ct != want {
		if log := ctx.Logger; log != nil {
			log.Printf("findGoogleGroupsThread: got error response with non-'text/html; charset=utf-8' Content-Type header %q\n", ct)
		}
		if mediaType, _, err := mime.ParseMediaType(ct); err != nil {
			return "", fmt.Errorf("bad Content-Type header %q: %v", ct, err)
		} else if mediaType != "text/html" {
			return "", fmt.Errorf("got media type %q, want %q", mediaType, "text/html")
		}
	}
	doc, err := html.Parse(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return "", err
	}
	var baseHref string
	var linkHref string
	var found bool
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "base" {
			baseHref = href(n)
		} else if n.Type == html.ElementNode && n.Data == "a" {
			linkHref = href(n)
		} else if n.Type == html.TextNode && n.Data == subject {
			// Found our link. Break out.
			found = true
			return
		}
		for c := n.FirstChild; c != nil && !found; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	if !found {
		// Thread not found on the first page.
		return "", nil
	}
	base, err := url.Parse(baseHref)
	if err != nil {
		return "", err
	}
	link, err := url.Parse(linkHref)
	if err != nil {
		return "", err
	}
	threadURL = base.ResolveReference(link).String()
	if !strings.HasPrefix(threadURL, announcementPrefix) {
		return "", fmt.Errorf("found URL %q, but it doesn't have the expected prefix %q", threadURL, announcementPrefix)
	}
	return threadURL, nil
}

func href(n *html.Node) string {
	for _, a := range n.Attr {
		if a.Key == "href" {
			return a.Val
		}
	}
	return ""
}

// renderMarkdown parses Markdown source
// and renders it to HTML and plain text.
//
// The Markdown specification and its various extensions are vast.
// Here we support a small, simple set of Markdown features
// that satisfies the needs of the announcement mail tasks.
func renderMarkdown(r io.Reader) (html, text string, _ error) {
	source, err := io.ReadAll(r)
	if err != nil {
		return "", "", err
	}
	// Configure a Markdown parser and HTML renderer fairly closely
	// to how it's done in x/website, just without raw HTML support
	// and other extensions we don't need for announcement emails.
	md := goldmark.New(
		goldmark.WithRendererOptions(goldmarkhtml.WithHardWraps()),
		goldmark.WithExtensions(
			extension.NewLinkify(extension.WithLinkifyAllowedProtocols([][]byte{[]byte("https:")})),
		),
	)
	doc := md.Parser().Parse(goldmarktext.NewReader(source))
	var htmlBuf, textBuf bytes.Buffer
	err = md.Renderer().Render(&htmlBuf, source, doc)
	if err != nil {
		return "", "", err
	}
	err = (markdownToTextRenderer{}).Render(&textBuf, source, doc)
	if err != nil {
		return "", "", err
	}
	return htmlBuf.String(), textBuf.String(), nil
}

// markdownToTextRenderer is a simple goldmark/renderer.Renderer implementation
// that renders Markdown to plain text for the needs of announcement mail tasks.
//
// It produces an output suitable for email clients that cannot (or choose not to)
// display the HTML version of the email. (It also helps a bit with the readability
// of our test data, since without a browser plain text is more readable than HTML.)
//
// The output is mostly plain text that doesn't preserve Markdown syntax (for example,
// `code` is rendered without backticks), though there is very lightweight formatting
// applied (links are written as "text <URL>").
//
// We can in theory choose to delete this renderer at any time if its maintenance costs
// start to outweight its benefits, since Markdown by definition is designed to be human
// readable and can be used as plain text without any processing.
type markdownToTextRenderer struct{}

func (markdownToTextRenderer) Render(w io.Writer, source []byte, n ast.Node) error {
	const debug = false
	if debug {
		n.Dump(source, 0)
	}

	var (
		markers []byte // Stack of list markers, from outermost to innermost.
	)
	walk := func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if n.Type() == ast.TypeBlock && n.PreviousSibling() != nil {
				// Print a blank line between block nodes.
				switch n.PreviousSibling().Kind() {
				default:
					fmt.Fprint(w, "\n\n")
				case ast.KindCodeBlock:
					// A code block always ends with a newline, so only need one more.
					fmt.Fprintln(w)
				}

				// If we're in a list, indent accordingly.
				if n.Kind() != ast.KindListItem {
					fmt.Fprint(w, strings.Repeat("\t", len(markers)))
				}
			}

			switch n := n.(type) {
			case *ast.Text:
				fmt.Fprintf(w, "%s", n.Text(source))

				// Print a line break.
				if n.SoftLineBreak() || n.HardLineBreak() {
					fmt.Fprintln(w)

					// If we're in a list, indent accordingly.
					fmt.Fprint(w, strings.Repeat("\t", len(markers)))
				}
			case *ast.CodeBlock:
				indent := strings.Repeat("\t", len(markers)+1) // Indent if in a list, plus one more since it's a code block.
				for i := 0; i < n.Lines().Len(); i++ {
					s := n.Lines().At(i)
					fmt.Fprint(w, indent, string(source[s.Start:s.Stop]))
				}
			case *ast.AutoLink:
				// Auto-links are printed as is in plain text.
				//
				// For example, the Markdown "https://go.dev/issue/123"
				// is rendered as plain text "https://go.dev/issue/123".
				fmt.Fprint(w, string(n.Label(source)))
			case *ast.List:
				// Push list marker on the stack.
				markers = append(markers, n.Marker)
			case *ast.ListItem:
				fmt.Fprintf(w, "%s%c\t", strings.Repeat("\t", len(markers)-1), markers[len(markers)-1])
			}
		} else {
			switch n := n.(type) {
			case *ast.Link:
				// Append the link's URL after its text.
				//
				// For example, the Markdown "[security policy](https://go.dev/security)"
				// is rendered as plain text "security policy <https://go.dev/security>".
				fmt.Fprintf(w, " <%s>", n.Destination)
			case *ast.List:
				// Pop list marker off the stack.
				markers = markers[:len(markers)-1]
			}

			if n.Type() == ast.TypeDocument && n.ChildCount() != 0 {
				// Print a newline at the end of the document, if it's not empty.
				fmt.Fprintln(w)
			}
		}
		return ast.WalkContinue, nil
	}
	return ast.Walk(n, walk)
}
func (markdownToTextRenderer) AddOptions(...renderer.Option) {}
