package connector

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

// UserLoginMetadata is one EduPage account. The password stays so that an expired session can be
// renewed without the user; EduPage has no tokens. Every access holds mu (the poller and bridgev2's
// own saves run in different goroutines), and so does marshalling.
type UserLoginMetadata struct {
	mu       sync.Mutex
	School   string `json:"school"`
	Username string `json:"username"`
	Password string `json:"password"`
	Session  string `json:"session"`
	UserID   string `json:"user_id"`
	// Backfilled: the first read (the school year) was made. Pending is when the oldest item not yet in
	// Matrix, or a notice still waiting, was posted (Unix seconds), so later polls read back to it.
	Backfilled bool  `json:"backfilled"`
	Pending    int64 `json:"pending,omitempty"`
	// Homework material already read, by the homework's superid.
	Materials map[string]*MaterialMeta `json:"materials,omitempty"`
}

// MaterialMeta is what a homework's cards said; Files are paths on the school's pages.
type MaterialMeta struct {
	Text  string     `json:"text,omitempty"`
	Files []FileMeta `json:"files,omitempty"`
}

type FileMeta struct {
	Name string `json:"name"`
	Src  string `json:"src"`
}

type userLoginMetadata UserLoginMetadata

func (m *UserLoginMetadata) MarshalJSON() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return json.Marshal((*userLoginMetadata)(m))
}

func (m *UserLoginMetadata) material(superID string) (*MaterialMeta, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mat, ok := m.Materials[superID]
	return mat, ok
}

func (m *UserLoginMetadata) setMaterial(superID string, mat *MaterialMeta) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Materials == nil {
		m.Materials = map[string]*MaterialMeta{}
	}
	m.Materials[superID] = mat
}

// MessageMetadata: Root is the thread's first message; Sig a hash of a notice's content, so a changed
// notice comes as an edit.
type MessageMetadata struct {
	Root int64  `json:"root,omitempty"`
	Sig  string `json:"sig,omitempty"`
	// A homework file part that came as a link because the download failed, and on the text part how
	// many times the files were tried; later polls try again a few times.
	Failed bool `json:"failed,omitempty"`
	Tries  int  `json:"tries,omitempty"`
}

const (
	schoolPortal = "school" // the "EduPage" room of notices
	schoolGhost  = "school" // the ghost that writes notices without a person
)

var reNotAlnum = regexp.MustCompile(`[^a-z0-9]`)

// makeUserID turns an EduPage owner like Ucitel123 into a ghost ID; Weft's Python bridge used the same,
// so ghosts keep their Matrix IDs. Without an owner the name's hash is used.
func makeUserID(owner, name string) networkid.UserID {
	if key := reNotAlnum.ReplaceAllString(strings.ToLower(owner), ""); key != "" {
		return networkid.UserID(key)
	}
	sum := sha1.Sum([]byte(name))
	return networkid.UserID(hex.EncodeToString(sum[:])[:12])
}

var rePerson = regexp.MustCompile(`^(ucitel|student|rodic)(?:only)?(\d+)$`)

// edupageID is the person a ghost stands for, as EduPage writes it (ucitel123 → Ucitel123), or "".
func edupageID(id networkid.UserID) string {
	m := rePerson.FindStringSubmatch(string(id))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1][:1]) + m[1][1:] + m[2]
}

func personPortal(id networkid.UserID) networkid.PortalID { return networkid.PortalID("p:" + id) }

func groupPortal(user string) networkid.PortalID { return networkid.PortalID("g:" + user) }

func portalPerson(id networkid.PortalID) (networkid.UserID, bool) {
	rest, ok := strings.CutPrefix(string(id), "p:")
	return networkid.UserID(rest), ok
}

func makeMessageID(id int64) networkid.MessageID {
	return networkid.MessageID(strconv.FormatInt(id, 10))
}

func parseMessageID(id networkid.MessageID) int64 {
	n, _ := strconv.ParseInt(string(id), 10, 64)
	return n
}

func loginID(school, userID string) networkid.UserLoginID {
	return networkid.UserLoginID(school + ":" + userID)
}
