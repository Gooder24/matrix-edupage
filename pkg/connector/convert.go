package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

// Extra content keys. Weft (the client this bridge comes from) reads them; other clients ignore them.
const (
	keyKind    = "sk.karchi.edupage.kind"    // the notice's EduPage type
	keyTo      = "sk.karchi.edupage.to"      // whom a thread's first message went to, as EduPage words it
	keyActions = "sk.karchi.edupage.actions" // [{label, url}]: EduPage's own button, opened there
	keySignIn  = "sk.karchi.edupage.signin"  // a sign-up's name, type, window and choice
)

var weekdays = []string{"nedeľa", "pondelok", "utorok", "streda", "štvrtok", "piatok", "sobota"}

// schoolPaths is where on EduPage each kind of notice is answered; importing it never confirms or signs anything.
var schoolPaths = map[string]string{"znamka": "znamky/", "signin": "signin/", "event": "user/", "homework": "user/"}

func itemText(it *edupage.Item, base string) string {
	text := it.Message()
	if atts := it.Attachments(); len(atts) > 0 {
		paths := make([]string, 0, len(atts))
		for p := range atts {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		lines := make([]string, len(paths))
		for i, p := range paths {
			lines[i] = "- " + atts[p] + ": " + resolve(base, p)
		}
		text += "\n\nPrílohy:\n" + strings.Join(lines, "\n")
	}
	if text = strings.TrimSpace(text); text == "" {
		return "(prázdna správa)"
	}
	return text
}

func resolve(base, path string) string {
	b, err := url.Parse(base + "/")
	if err != nil {
		return path
	}
	u, err := b.Parse(path)
	if err != nil {
		return path
	}
	return u.String()
}

func (ec *EduPageClient) base(school string) string {
	return strings.ReplaceAll(ec.Main.Config.EduPageURL, "{school}", school)
}

var reSchoolName = regexp.MustCompile(`^[a-z0-9-]+$`)

// actions keeps EduPage's own button (like "Sign up") as a link to its page, never doing it.
func (ec *EduPageClient) actions(it *edupage.Item, school string) []map[string]string {
	link, _ := it.Data()["edupageLink"].(map[string]any)
	raw, _ := link["url"].(string)
	name, _ := link["name"].(string)
	if raw == "" || strings.TrimSpace(name) == "" || !reSchoolName.MatchString(school) {
		return nil
	}
	base := ec.base(school)
	full := resolve(base, raw)
	b, _ := url.Parse(base)
	u, err := url.Parse(full)
	if err != nil || b == nil || u.Scheme != b.Scheme || u.Host != b.Host {
		return nil
	}
	return []map[string]string{{"label": strings.TrimSpace(name), "url": full}}
}

func (ec *EduPageClient) convertMessage(it *edupage.Item, root int64, school string) *bridgev2.ConvertedMessage {
	id := it.ID.Int()
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: itemText(it, ec.base(school))}
	extra := map[string]any{}
	if a := ec.actions(it, school); a != nil {
		extra[keyActions] = a
	}
	cm := &bridgev2.ConvertedMessage{}
	if root != id {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: makeMessageID(root)}
	} else if it.UserName != "" {
		extra[keyTo] = string(it.UserName)
	}
	cm.Parts = []*bridgev2.ConvertedMessagePart{{
		Type: event.EventMessage, Content: content, Extra: extra,
		DBMetadata: &MessageMetadata{Root: root},
	}}
	return cm
}

// notice is a school announcement ready to post: its text and files, and a hash of both.
type notice struct {
	kind    string
	text    string
	actions []map[string]string
	signIn  map[string]any
	files   []FileMeta
	base    string
	sig     string
}

func localLabel(stamp string) string {
	t, err := time.ParseInLocation(time.DateTime, stamp, edupage.Location)
	if err != nil {
		return stamp
	}
	return fmt.Sprintf("%d. %d. %s", t.Day(), int(t.Month()), t.Format("15:04"))
}

// signIn is what a sign-up is, when it is open and what the user chose; EduPage's text says only the dates.
func signIn(it *edupage.Item) map[string]any {
	info := map[string]any{}
	for k, src := range map[string]string{"name": "name", "type": "typeName", "from": "time_from", "to": "time_to"} {
		if v := strings.TrimSpace(it.DataString(src)); v != "" {
			info[k] = v
		}
	}
	var chosen []string
	names, _ := it.Data()["targetNames"].([]any)
	for _, n := range names {
		if s, ok := n.(string); ok && strings.TrimSpace(s) != "" {
			chosen = append(chosen, strings.TrimSpace(s))
		}
	}
	if len(chosen) > 0 {
		info["chosen"] = chosen
		if c := it.DataString("confirmTime"); c != "" {
			info["confirmed"] = c
		}
	}
	return info
}

func signInText(info map[string]any) string {
	str := func(k string) string { s, _ := info[k].(string); return s }
	name := str("name")
	if name == "" {
		name = "bez názvu"
	}
	lines := []string{"Prihlasovanie: " + name}
	if t := str("type"); t != "" {
		lines = append(lines, t)
	}
	if str("from") != "" || str("to") != "" {
		lines = append(lines, fmt.Sprintf("Otvorené %s – %s", localLabel(str("from")), localLabel(str("to"))))
	}
	if chosen, ok := info["chosen"].([]string); ok {
		when := ""
		if c := str("confirmed"); c != "" {
			when = " (" + localLabel(c) + ")"
		}
		lines = append(lines, "Zvolené: "+strings.Join(chosen, ", ")+when)
	}
	return strings.Join(lines, "\n")
}

func homeworkDue(it *edupage.Item) string {
	due, err := time.Parse(time.DateOnly, it.DataString("date"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("Termín: %s %d. %d.", weekdays[due.Weekday()], due.Day(), int(due.Month()))
}

func (ec *EduPageClient) buildNotice(it *edupage.Item, school string, material *MaterialMeta) *notice {
	n := &notice{kind: string(it.Type), base: ec.base(school)}
	if it.Type == "confirmation" {
		n.text = "Potvrdené prečítanie: " + it.DataString("receipt")
	} else {
		n.text = strings.Join(strings.Fields(it.Message()), " ")
		if n.text == "" {
			n.text = n.kind
		}
		if it.Type == "signin" {
			n.signIn = signIn(it)
			n.text = signInText(n.signIn)
		}
		if it.Type == "homework" {
			parts := []string{n.text}
			for _, p := range []string{edupage.HTMLText(it.DataString("popis")), materialText(material), homeworkDue(it)} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			n.text = strings.Join(parts, "\n\n")
		}
		if path, ok := schoolPaths[n.kind]; ok && reSchoolName.MatchString(school) {
			n.text += "\n\n" + n.base + "/" + path
		}
	}
	n.actions = ec.actions(it, school)
	if material != nil {
		n.files = material.Files
	}
	raw, _ := json.Marshal([]any{n.kind, n.text, n.actions, n.signIn, n.files})
	sum := sha256.Sum256(raw)
	n.sig = hex.EncodeToString(sum[:])
	return n
}

func materialText(m *MaterialMeta) string {
	if m == nil {
		return ""
	}
	return m.Text
}

// fileTries is how often a homework file is downloaded before its link stays.
const fileTries = 3

// retrySuffix marks the signature of a notice whose files did not all come across.
const retrySuffix = "+files"

// noticeUpToDate reports whether a bridged notice needs no edit: the same signature, and no file
// left to try again.
func noticeUpToDate(parts []*database.Message, sig string) bool {
	for _, p := range parts {
		if p.PartID == "" {
			meta := p.Metadata.(*MessageMetadata)
			return meta.Sig == sig || (meta.Sig == sig+retrySuffix && meta.Tries >= fileTries)
		}
	}
	return false
}

func (n *notice) content() (*event.MessageEventContent, map[string]any) {
	extra := map[string]any{keyKind: n.kind}
	if n.actions != nil {
		extra[keyActions] = n.actions
	}
	if len(n.signIn) > 0 {
		extra[keySignIn] = n.signIn
	}
	return &event.MessageEventContent{MsgType: event.MsgNotice, Body: n.text}, extra
}

func (ec *EduPageClient) convertNotice(ctx context.Context, intent bridgev2.MatrixAPI, n *notice, replyTo int64) (*bridgev2.ConvertedMessage, error) {
	content, extra := n.content()
	textMeta := &MessageMetadata{Sig: n.sig}
	cm := &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
		Type: event.EventMessage, Content: content, Extra: extra, DBMetadata: textMeta,
	}}}
	if replyTo != 0 {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: makeMessageID(replyTo)}
	}
	for i, f := range n.files {
		part, failed := ec.homeworkFile(ctx, intent, f, n.base)
		part.ID = networkid.PartID(fmt.Sprintf("file-%d", i))
		part.DBMetadata = &MessageMetadata{Failed: failed}
		if failed {
			textMeta.Sig, textMeta.Tries = n.sig+retrySuffix, 1
		}
		cm.Parts = append(cm.Parts, part)
	}
	return cm, nil
}

// homeworkFile is a material file as a Matrix file, or as a link to it when the download fails.
func (ec *EduPageClient) homeworkFile(ctx context.Context, intent bridgev2.MatrixAPI, f FileMeta, base string) (*bridgev2.ConvertedMessagePart, bool) {
	part, err := ec.filePart(ctx, intent, f, base)
	if err == nil {
		return part, false
	}
	zerolog.Ctx(ctx).Warn().Err(err).Str("file", f.Name).Msg("Failed to bridge a homework file")
	return &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: &event.MessageEventContent{
		MsgType: event.MsgNotice, Body: fmt.Sprintf("%s: %s", f.Name, resolve(base, f.Src)),
	}}, true
}

// filePart copies a material file into Matrix; EduPage serves it only to a signed-in session.
func (ec *EduPageClient) filePart(ctx context.Context, intent bridgev2.MatrixAPI, f FileMeta, base string) (*bridgev2.ConvertedMessagePart, error) {
	var data []byte
	var mimeType string
	err := ec.withSession(ctx, func(c *edupage.Client) (err error) {
		data, mimeType, err = c.Download(ctx, c.Resolve(f.Src), ec.Main.Config.MaxFileSize)
		return err
	})
	if err != nil {
		return nil, err
	}
	name := f.Name
	if name == "" {
		name = "subor"
	}
	mxc, file, err := intent.UploadMedia(ctx, "", data, name, mimeType)
	if err != nil {
		return nil, err
	}
	content := &event.MessageEventContent{MsgType: event.MsgFile, Body: name, URL: mxc, File: file,
		Info: &event.FileInfo{MimeType: mimeType, Size: len(data)}}
	if strings.HasPrefix(mimeType, "image/") {
		content.MsgType = event.MsgImage
		if w, h, ok := imageSize(data); ok {
			content.Info.Width, content.Info.Height = w, h
		}
	}
	return &bridgev2.ConvertedMessagePart{Type: event.EventMessage, Content: content}, nil
}

// convertNoticeEdit changes a notice's text and tries again the files that came only as links.
func (ec *EduPageClient) convertNoticeEdit(ctx context.Context, _ *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, n *notice) (*bridgev2.ConvertedEdit, error) {
	var text *database.Message
	for _, p := range existing {
		if p.PartID == "" {
			text = p
		}
	}
	if text == nil {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	edit := &bridgev2.ConvertedEdit{}
	meta := text.Metadata.(*MessageMetadata)
	stillFailed := false
	for _, p := range existing {
		var i int
		pm := p.Metadata.(*MessageMetadata)
		if _, err := fmt.Sscanf(string(p.PartID), "file-%d", &i); err != nil || !pm.Failed || i >= len(n.files) {
			continue
		}
		part, failed := ec.homeworkFile(ctx, intent, n.files[i], n.base)
		if failed {
			stillFailed = true
			continue
		}
		pm.Failed = false
		edit.ModifiedParts = append(edit.ModifiedParts, &bridgev2.ConvertedEditPart{Part: p, Type: event.EventMessage, Content: part.Content})
	}
	// The text part is always edited too: bridgev2 saves the metadata only of the parts it edits.
	if stillFailed {
		meta.Sig = n.sig + retrySuffix
		meta.Tries++
	} else {
		meta.Sig, meta.Tries = n.sig, 0
	}
	content, extra := n.content()
	edit.ModifiedParts = append(edit.ModifiedParts, &bridgev2.ConvertedEditPart{Part: text, Type: event.EventMessage, Content: content, Extra: extra})
	return edit, nil
}

// imageSize reads a PNG, GIF or JPEG's pixel size.
func imageSize(data []byte) (w, h int, ok bool) {
	be := func(b []byte) int {
		n := 0
		for _, x := range b {
			n = n<<8 | int(x)
		}
		return n
	}
	switch {
	case len(data) >= 24 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return be(data[16:20]), be(data[20:24]), true
	case len(data) >= 10 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return int(data[6]) | int(data[7])<<8, int(data[8]) | int(data[9])<<8, true
	case len(data) >= 2 && data[0] == 0xFF && data[1] == 0xD8:
		for i := 2; i+9 <= len(data) && data[i] == 0xFF; {
			marker, length := data[i+1], be(data[i+2:i+4])
			if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
				return be(data[i+7 : i+9]), be(data[i+5 : i+7]), true
			}
			i += 2 + length
		}
	}
	return 0, 0, false
}
