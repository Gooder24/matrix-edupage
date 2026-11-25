package edupage

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Str is a value EduPage sends as a string, a number or null.
type Str string

func (s *Str) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = ""
		return nil
	}
	var v string
	if json.Unmarshal(b, &v) == nil {
		*s = Str(v)
		return nil
	}
	*s = Str(strings.Trim(string(b), `"`))
	return nil
}

func (s Str) Int() int64 {
	n, _ := strconv.ParseInt(string(s), 10, 64)
	return n
}

// Item is one row of the timeline: a message, a reply or a notice.
type Item struct {
	ID         Str             `json:"timelineid"`
	Type       Str             `json:"typ"`
	Added      Str             `json:"cas_pridania"`
	Timestamp  Str             `json:"timestamp"`
	Owner      Str             `json:"vlastnik"`
	OwnerName  Str             `json:"vlastnik_meno"`
	User       Str             `json:"user"`
	UserName   Str             `json:"user_meno"`
	Text       Str             `json:"text"`
	RawData    json.RawMessage `json:"data"`
	ReplyTo    Str             `json:"reakcia_na"`
	ParentID   Str             `json:"ineid"`
	Helper     Str             `json:"pomocny_zaznam"`
	Removed    Str             `json:"removed"`
	Homework   *Homework       `json:"-"`
	parsedData map[string]any
}

// Homework is the homework list's row for a homework notice: what its material is.
type Homework struct {
	Type        Str `json:"typ"`
	TestID      Str `json:"testid"`
	SuperID     Str `json:"e_superid"`
	Cards       any `json:"etestCards"`
	AnswerCards any `json:"etestAnswerCards"`
}

// Timeline holds the timeline rows and the homework list of one read.
type Timeline struct {
	Items     []Item     `json:"timelineItems"`
	Homeworks []Homework `json:"homeworks"`
}

// Location is the time zone EduPage's wall-clock times are in.
var Location = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Bratislava")
	if err != nil {
		return time.Local
	}
	return loc
}()

// Time is when the item was added.
func (it *Item) Time() time.Time {
	stamp := string(it.Added)
	if stamp == "" {
		stamp = string(it.Timestamp)
	}
	t, err := time.ParseInLocation(time.DateTime, stamp, Location)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Data is the item's data object (EduPage sends it as an object or as a JSON string).
func (it *Item) Data() map[string]any {
	if it.parsedData != nil {
		return it.parsedData
	}
	var raw any
	_ = json.Unmarshal(it.RawData, &raw)
	if s, ok := raw.(string); ok {
		raw = nil
		_ = json.Unmarshal([]byte(s), &raw)
	}
	it.parsedData, _ = raw.(map[string]any)
	if it.parsedData == nil {
		it.parsedData = map[string]any{}
	}
	return it.parsedData
}

func (it *Item) DataString(key string) string {
	switch v := it.Data()[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (it *Item) IsRemoved() bool { return it.Removed == "1" }

// Message is the item's text: important messages keep the body in messageContent, homework its title.
func (it *Item) Message() string {
	text := it.DataString("messageContent")
	if strings.TrimSpace(text) == "" {
		text = string(it.Text)
	}
	if strings.TrimSpace(text) == "" && it.Type == "homework" {
		text = it.DataString("nazov")
	}
	return strings.TrimSpace(text)
}

// Attachments maps the item's file paths on EduPage to their names.
func (it *Item) Attachments() map[string]string {
	raw, _ := it.Data()["attachements"].(map[string]any)
	out := make(map[string]string, len(raw))
	for path, name := range raw {
		if s, ok := name.(string); ok {
			out[path] = s
		}
	}
	return out
}

// HomeworkSuperID links a homework notice to its row in the homework list.
func (it *Item) HomeworkSuperID() string {
	if it.Type != "homework" {
		return ""
	}
	return it.DataString("superid")
}

// IsMine reports whether the signed-in person wrote the item.
func (it *Item) IsMine(userID string) bool {
	return userID != "" && strings.ReplaceAll(string(it.Owner), "Only", "") == userID
}

// Timeline reads the timeline since a day, the same call edupage.org makes; tab "" is everything
// except the messages tab, "messages" is that tab.
func (c *Client) Timeline(ctx context.Context, since time.Time, tab string) (*Timeline, error) {
	q := "module=todo&filterTab=&akcia=getData"
	if tab != "" {
		q += "&filterTab=" + url.QueryEscape(tab)
	}
	_, data, err := c.postForm(ctx, c.Base()+"/timeline/?"+q, "datefrom="+since.Format(time.DateOnly))
	if err != nil {
		return nil, err
	}
	var tl struct {
		Items     *[]Item    `json:"timelineItems"`
		Homeworks []Homework `json:"homeworks"`
	}
	if json.Unmarshal(data, &tl) != nil || tl.Items == nil {
		return nil, ErrSessionExpired // the login page, not a successful empty read
	}
	return &Timeline{Items: *tl.Items, Homeworks: tl.Homeworks}, nil
}

// Everything reads both tabs since a day: the message tab plus all other rows, one per item id,
// with each homework notice's row from the homework list.
func (c *Client) Everything(ctx context.Context, since time.Time) ([]Item, error) {
	all, err := c.Timeline(ctx, since, "")
	if err != nil {
		return nil, err
	}
	messages, err := c.Timeline(ctx, since, "messages")
	if err != nil {
		return nil, err
	}
	homeworks := map[string]*Homework{}
	for i := range all.Homeworks {
		homeworks[string(all.Homeworks[i].SuperID)] = &all.Homeworks[i]
	}
	byID := map[string]int{}
	var out []Item
	for _, list := range [][]Item{all.Items, messages.Items} {
		for _, it := range list {
			if i, ok := byID[string(it.ID)]; ok {
				out[i] = it
				continue
			}
			byID[string(it.ID)] = len(out)
			out = append(out, it)
		}
	}
	for i := range out {
		out[i].Homework = homeworks[out[i].HomeworkSuperID()]
	}
	return out, nil
}

// File is one file of a homework's material card.
type File struct{ Name, Src string }

// Material is a homework's card text and files.
type Material struct {
	Text  string
	Files []File
}

var (
	reBreak = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6]|tr)>`)
	reTag   = regexp.MustCompile(`<[^>]+>`)
)

// HTMLText is the plain text of EduPage rich text, paragraphs and list items on their own lines.
func HTMLText(markup string) string {
	text := html.UnescapeString(reTag.ReplaceAllString(reBreak.ReplaceAllString(markup, "\n"), ""))
	text = strings.ReplaceAll(text, " ", " ")
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func nonZero(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case float64:
		return n != 0
	case string:
		return n != "" && n != "0"
	case []any:
		return len(n) > 0
	case map[string]any:
		return len(n) > 0
	}
	return true
}

// Material reads a homework's cards the way edupage.org's player does. Only plain homework whose
// cards ask for no answer: reading it marks nothing as seen or started. Others give an empty Material.
func (c *Client) Material(ctx context.Context, hw *Homework) (*Material, error) {
	// Like edupage-api: cards only when EduPage says outright that none of them asks for an answer.
	if hw == nil || hw.Type != "etesthw|hw" || !nonZero(hw.Cards) || hw.AnswerCards == nil || nonZero(hw.AnswerCards) {
		return &Material{}, nil
	}
	form := url.Values{"testid": {string(hw.TestID)}, "superid": {string(hw.SuperID)}, "pridelenieid": {""}, "vysledokid": {""}}
	_, data, err := c.postForm(ctx, c.Base()+"/elearning/?cmd=MaterialPlayer&akcia=getETestData", form.Encode())
	if err != nil {
		return nil, err
	}
	var answer struct {
		MaterialData struct {
			CardIDs   []Str `json:"cardids"`
			CardsData map[string]struct {
				Content string `json:"content"`
			} `json:"cardsData"`
		} `json:"materialData"`
	}
	if err = json.Unmarshal(data, &answer); err != nil {
		return nil, ErrSessionExpired
	}
	ids := answer.MaterialData.CardIDs
	if len(ids) == 0 {
		for id := range answer.MaterialData.CardsData {
			ids = append(ids, Str(id))
		}
	}
	out := &Material{}
	var texts []string
	for _, id := range ids {
		var widget map[string]any
		if json.Unmarshal([]byte(answer.MaterialData.CardsData[string(id)].Content), &widget) != nil {
			continue
		}
		walkWidgets(widget, func(w map[string]any) {
			props, _ := w["props"].(map[string]any)
			str := func(k string) string { s, _ := props[k].(string); return s }
			switch w["widgetClass"] {
			case "TitleETestWidget":
				texts = append(texts, HTMLText(str("text")))
			case "TextETestWidget":
				// A secured card keeps its text only in the parsed copy.
				t := str("htmlText")
				if t == "" {
					t = str("_parsedHtmlText")
				}
				texts = append(texts, HTMLText(t))
			case "FileETestWidget":
				files, _ := props["files"].([]any)
				for _, f := range files {
					fm, _ := f.(map[string]any)
					src, _ := fm["src"].(string)
					name, _ := fm["name"].(string)
					if src != "" {
						out.Files = append(out.Files, File{Name: name, Src: src})
					}
				}
			}
		})
	}
	for _, t := range texts {
		if t != "" {
			if out.Text != "" {
				out.Text += "\n\n"
			}
			out.Text += t
		}
	}
	return out, nil
}

func walkWidgets(w map[string]any, fn func(map[string]any)) {
	fn(w)
	children, _ := w["widgets"].([]any)
	for _, ch := range children {
		if m, ok := ch.(map[string]any); ok {
			walkWidgets(m, fn)
		}
	}
}

// Resolve turns a path on the school's pages into a full address.
func (c *Client) Resolve(path string) string {
	base, err := url.Parse(c.Base() + "/")
	if err != nil {
		return path
	}
	u, err := base.Parse(path)
	if err != nil {
		return path
	}
	return u.String()
}
