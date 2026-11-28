package edupage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// school is a made-up school: classes 7 (class teacher 11) and 8, pupils 501 and 502 in 7 and 503 in 8,
// one who left, a parent of 502, two subject groups and teachers 11 and 12 and one who left.
func school(userID string, extra map[string]any) map[string]any {
	data := map[string]any{"userid": userID, "dbi": map[string]any{
		"teachers": map[string]any{
			"11": map[string]any{"firstname": "Bea", "lastname": "Teacher"},
			"12": map[string]any{"firstname": "Cyril", "lastname": "Teacher"},
			"13": map[string]any{"firstname": "Dora", "lastname": "Teacher", "dateto": "2001-06-30"},
		},
		"classes": map[string]any{
			"7": map[string]any{"short": "7.A", "teacherid": "11"},
			"8": map[string]any{"short": "8.B", "teacherid": "12"},
		},
		"students": map[string]any{
			"501": map[string]any{"firstname": "Alex", "lastname": "Pupil", "classid": "7"},
			"502": map[string]any{"firstname": "Bo", "lastname": "Classmate", "classid": "7"},
			"503": map[string]any{"firstname": "Cid", "lastname": "Other", "classid": 8.0},
			"504": map[string]any{"firstname": "Gone", "lastname": "Pupil", "classid": "7", "isOut": true},
		},
		"parents": map[string]any{
			"601": map[string]any{"firstname": "Pat", "lastname": "Parent", "studentids": []any{"502"}},
		},
		"plans": map[string]any{
			"31": map[string]any{"triedaMeno": "7.A", "predmetMeno": "Mathematics", "students": []any{"501", "502"}},
			"32": map[string]any{"triedaid": "8", "predmetMeno": "Art", "zlozka": "Drawing", "students": []any{503.0}},
		},
	}}
	for k, v := range extra {
		data[k] = v
	}
	return data
}

func ids(rs []Recipient) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

func TestRecipientsByRole(t *testing.T) {
	c := New("", time.Second)
	c.Data = school("Student501", nil)
	if c.Role() != RoleStudent {
		t.Errorf("role %s", c.Role())
	}
	pupil := c.Recipients()
	if got, want := ids(pupil), []string{"StudTrieda7", "StudPlan31", "Ucitel11", "Ucitel12", "StudentOnly502"}; !slices.Equal(got, want) {
		t.Errorf("a pupil gets %v, want %v", got, want)
	}
	if pupil[0].Name != "7.A" || pupil[0].Parents != "Trieda7" || pupil[0].Count != 2 {
		t.Errorf("class %+v", pupil[0])
	}
	if pupil[1].Name != "7.A · Mathematics" || pupil[1].Parents != "Plan31" || pupil[1].Count != 2 {
		t.Errorf("group %+v", pupil[1])
	}
	if s := pupil[4]; s.Parents != "Student502" || s.Class != "7.A" || s.Name != "Bo Classmate" {
		t.Errorf("classmate %+v", s)
	}
	// EduPage's own list of the pupil's groups wins over the plans' pupils.
	c.Data = school("Student501", map[string]any{"userGroups": []any{"StudPlan32"}})
	if got := ids(c.Recipients()); !slices.Contains(got, "StudPlan32") || slices.Contains(got, "StudPlan31") {
		t.Errorf("a pupil's userGroups: %v", got)
	}

	c.Data = school("Ucitel11", nil)
	if c.Role() != RoleTeacher {
		t.Errorf("role %s", c.Role())
	}
	teacher := c.Recipients()
	want := []string{"StudTrieda7", "StudTrieda8", "StudPlan31", "StudPlan32", "Ucitel12", "StudentOnly501", "StudentOnly502",
		"StudentOnly503", "Rodic601", "StudentOnly*", "Ucitel*"}
	if got := ids(teacher); !slices.Equal(got, want) {
		t.Errorf("a teacher gets %v, want %v", got, want)
	}
	if !teacher[0].Own || teacher[1].Own || teacher[3].Name != "8.B · Art - Drawing" {
		t.Errorf("own class or group name: %+v %+v %+v", teacher[0], teacher[1], teacher[3])
	}
	if p := teacher[8]; !slices.Equal(p.Children, []string{"Bo Classmate"}) || p.Class != "7.A" || p.Parents != "" {
		t.Errorf("parent %+v", p)
	}
	if all := teacher[9]; all.Parents != "Student*" {
		t.Errorf("all pupils %+v", all)
	}
	offered := Offered(teacher)
	if offered["trieda7"] != "Trieda7" || offered["student*"] != "Student*" || offered["ucitel11"] != "" {
		t.Errorf("offered %v", offered)
	}

	c.Data = school("Rodic601", nil)
	if got := ids(c.Recipients()); c.Role() != RoleParent || !slices.Equal(got, []string{"Ucitel11", "Ucitel12"}) {
		t.Errorf("a parent (%s) gets %v", c.Role(), got)
	}
}

func TestSendMessageOptions(t *testing.T) {
	var forms []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := DecodeRequestBody(string(body))
		if err != nil {
			t.Error(err)
		}
		forms = append(forms, form.Encode())
		_, _ = io.WriteString(w, `{"changes":[{"timelineid":"42"}]}`)
	}))
	defer srv.Close()
	c := New(srv.URL+"/{school}", time.Second)
	c.School = "s"
	for _, opts := range []*Options{nil, {Important: true, Replies: "author"}, {Replies: "none"}, {Replies: "bogus"}} {
		if id, err := c.SendMessage(context.Background(), "StudTrieda7;Rodic601", "hi", nil, opts); err != nil || id != 42 {
			t.Fatalf("%d %v", id, err)
		}
	}
	for i, want := range []string{
		"receipt=0&selectedUser=StudTrieda7%3BRodic601",
		"receipt=1&repliesDisabled=0&repliesToAllDisabled=1",
		"receipt=0&repliesDisabled=1&repliesToAllDisabled=1",
		"receipt=0&repliesDisabled=0&repliesToAllDisabled=0",
	} {
		for _, part := range strings.Split(want, "&") {
			if !strings.Contains("&"+forms[i]+"&", "&"+part+"&") {
				t.Errorf("message %d lacks %s: %s", i, part, forms[i])
			}
		}
	}
	if strings.Contains(forms[0], "replies") {
		t.Errorf("a message without options carries a teacher's fields: %s", forms[0])
	}
}
