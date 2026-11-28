package edupage

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Recipient is someone or something a new message can go to, under EduPage's own ids: StudentOnly12 is
// a pupil alone and Student12 the pupil with their parents, StudTrieda/Trieda a class, StudPlan/Plan a
// subject group, Ucitel* every teacher and StudentOnly*/Student* every pupil of the school.
type Recipient struct {
	ID string
	// Parents is the id that also reaches the parents, where there is one.
	Parents string
	Name    string
	Kind    string // class, group, teacher, student, parent or all
	// Own: a class the signed-in teacher is the class teacher of.
	Own bool
	// Class is a pupil's or a parent's child's class; Children a parent's children.
	Class    string
	Children []string
	// Count is how many pupils a class or group has, when EduPage says.
	Count int
}

// Roles as Role gives them.
const (
	RoleStudent = "student"
	RoleTeacher = "teacher"
	RoleParent  = "parent"
)

// Role is what the signed-in person is at the school; an account EduPage does not name is a pupil's.
func (c *Client) Role() string {
	m := reUserID.FindStringSubmatch(c.UserID())
	if m == nil {
		return RoleStudent
	}
	switch m[1] {
	case "Ucitel":
		return RoleTeacher
	case "Rodic":
		return RoleParent
	}
	return RoleStudent
}

// text reads an id or a name the way EduPage's data may give it: a string or a number.
func text(v any) string {
	switch v := v.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

func truthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != "" && v != "0"
	}
	return v != nil
}

var reDigits = regexp.MustCompile(`^-?\d+$`)

// Recipients lists whom the signed-in person can start a message to, from the school's own data. A
// pupil gets their class, their subject groups, their classmates and the teachers; a teacher every
// class, every group, every pupil and parent, the other teachers, all teachers and all pupils; a parent
// the teachers. What a school does not allow EduPage still refuses when the message is sent.
func (c *Client) Recipients() []Recipient {
	dbi, _ := c.Data["dbi"].(map[string]any)
	table := func(name string) map[string]map[string]any {
		out := map[string]map[string]any{}
		raw, _ := dbi[name].(map[string]any)
		for k, v := range raw {
			if row, ok := v.(map[string]any); ok {
				out[k] = row
			}
		}
		return out
	}
	teachers, students, classes, parents := table("teachers"), table("students"), table("classes"), table("parents")
	subjects, plans := table("subjects"), table("plans")
	today := time.Now().In(Location).Format(time.DateOnly)
	current := func(p map[string]any) bool {
		to := text(p["dateto"])
		return !truthy(p["isOut"]) && (to == "" || to >= today)
	}
	name := func(p map[string]any) string {
		return strings.TrimSpace(text(p["firstname"]) + " " + text(p["lastname"]))
	}
	short := func(cid string) string {
		if s := text(classes[cid]["short"]); s != "" {
			return s
		}
		return text(classes[cid]["name"])
	}
	role, me := c.Role(), ""
	if m := reUserID.FindStringSubmatch(c.UserID()); m != nil {
		me = m[2]
	}
	var out []Recipient
	add := func(r Recipient) {
		if r.Name != "" {
			out = append(out, r)
		}
	}
	for tid, t := range teachers {
		if reDigits.MatchString(tid) && current(t) && (role != RoleTeacher || tid != me) {
			add(Recipient{ID: "Ucitel" + tid, Name: name(t), Kind: "teacher"})
		}
	}
	if role == RoleParent {
		sortRecipients(out)
		return out
	}
	mine := ""
	if role == RoleStudent {
		mine = text(students[me]["classid"])
	}
	for cid, cl := range classes {
		if role != RoleTeacher && (mine == "" || cid != mine) {
			continue
		}
		n := 0
		for _, st := range students {
			if current(st) && text(st["classid"]) == cid {
				n++
			}
		}
		own := role == RoleTeacher && (text(cl["teacherid"]) == me || text(cl["teacher2id"]) == me)
		add(Recipient{ID: "StudTrieda" + cid, Parents: "Trieda" + cid, Name: short(cid), Kind: "class", Own: own, Count: n})
	}
	// A pupil's groups are only those their account belongs to: EduPage's userGroups, else the plan's pupils.
	member := map[string]bool{}
	groups, _ := c.Data["userGroups"].([]any)
	for _, g := range groups {
		member[text(g)] = true
	}
	for pid, plan := range plans {
		var pupils []string
		list, isList := plan["students"].([]any)
		for _, p := range list {
			pupils = append(pupils, text(p))
		}
		if role == RoleStudent && (len(member) > 0 && !member["StudPlan"+pid] || len(member) == 0 && !slices.Contains(pupils, me)) {
			continue
		}
		subject := text(plan["predmetMeno"])
		if subject == "" {
			subject = text(plan["nazov"])
		}
		if subject == "" {
			subject = text(subjects[text(plan["predmetid"])]["name"])
		}
		if subject == "" {
			continue
		}
		if part := text(plan["zlozka"]); part != "" {
			subject += " - " + part
		}
		cls := text(plan["triedaMeno"])
		if cls == "" {
			cls = short(text(plan["triedaid"]))
		}
		title := subject // named as EduPage names them: "III. A · Slovenský jazyk a literatúra - Literatúra"
		if cls != "" {
			title = cls + " · " + subject
		}
		r := Recipient{ID: "StudPlan" + pid, Parents: "Plan" + pid, Name: title, Kind: "group"}
		if isList {
			r.Count = len(list)
		}
		add(r)
	}
	for sid, st := range students {
		cid := text(st["classid"])
		if reDigits.MatchString(sid) && current(st) && sid != me && (role == RoleTeacher || mine != "" && cid == mine) {
			add(Recipient{ID: "StudentOnly" + sid, Parents: "Student" + sid, Name: name(st), Kind: "student", Class: short(cid)})
		}
	}
	if role == RoleTeacher {
		for rid, p := range parents {
			if !reDigits.MatchString(rid) {
				continue
			}
			r := Recipient{ID: "Rodic" + rid, Name: name(p), Kind: "parent"}
			kids, _ := p["studentids"].([]any)
			for _, k := range kids {
				if st, ok := students[text(k)]; ok {
					r.Children = append(r.Children, name(st))
					if r.Class == "" {
						r.Class = short(text(st["classid"]))
					}
				}
			}
			add(r)
		}
		add(Recipient{ID: "Ucitel*", Name: "All teachers", Kind: "all"})
		add(Recipient{ID: "StudentOnly*", Parents: "Student*", Name: "All pupils", Kind: "all"})
	}
	sortRecipients(out)
	return out
}

var kindOrder = map[string]int{"class": 0, "group": 1, "teacher": 2, "student": 3, "parent": 4, "all": 5}

func sortRecipients(rs []Recipient) {
	slices.SortStableFunc(rs, func(a, b Recipient) int {
		if d := kindOrder[a.Kind] - kindOrder[b.Kind]; d != 0 {
			return d
		}
		if a.Own != b.Own {
			if a.Own {
				return -1
			}
			return 1
		}
		if d := strings.Compare(a.Name, b.Name); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// Offered maps each id Recipients offers, in lower case, to EduPage's own spelling of it: the
// recipients' ids and the ids that add their parents.
func Offered(rs []Recipient) map[string]string {
	out := map[string]string{}
	for _, r := range rs {
		out[strings.ToLower(r.ID)] = r.ID
		if r.Parents != "" {
			out[strings.ToLower(r.Parents)] = r.Parents
		}
	}
	return out
}

// Options are a teacher's choices on edupage.org's "New message" form: Important asks for a read
// confirmation, Replies lets everyone answer everyone ("all", the default), only the author ("author")
// or nobody ("none"). EduPage offers them to teachers only.
type Options struct {
	Important bool   `json:"important,omitempty"`
	Replies   string `json:"replies,omitempty"`
}

func (o *Options) fields() []Pair {
	if o == nil {
		return []Pair{{"receipt", "0"}}
	}
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	replies := o.Replies
	if replies != "author" && replies != "none" {
		replies = "all"
	}
	return []Pair{{"receipt", flag(o.Important)}, {"repliesDisabled", flag(replies == "none")}, {"repliesToAllDisabled", flag(replies != "all")}}
}
