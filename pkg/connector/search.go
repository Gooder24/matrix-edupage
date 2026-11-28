package connector

import (
	"context"
	"sort"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

func (ec *EduPageClient) contact(id networkid.UserID, name string) *bridgev2.ResolveIdentifierResponse {
	return &bridgev2.ResolveIdentifierResponse{
		UserID:   id,
		UserInfo: &bridgev2.UserInfo{Name: ptr.Ptr(name), IsBot: ptr.Ptr(false)},
	}
}

// recipientContact is one of the contact list: a person's ID is their ghost's, a class's or a group's
// the recipient id in lower case (studtrieda7, studplan12, ucitel*), which create_group takes. Its
// identifiers are EduPage's ids, the second one with the parents.
func (ec *EduPageClient) recipientContact(r edupage.Recipient) *bridgev2.ResolveIdentifierResponse {
	name := r.Name
	switch {
	case len(r.Children) > 0:
		name += " (parent of " + strings.Join(r.Children, ", ") + ")"
	case r.Kind == "student" && r.Class != "":
		name += " (" + r.Class + ")"
	}
	c := ec.contact(networkid.UserID(strings.ToLower(r.ID)), name)
	c.UserInfo.Identifiers = []string{"edupage:" + r.ID}
	if r.Parents != "" {
		c.UserInfo.Identifiers = append(c.UserInfo.Identifiers, "edupage:"+r.Parents)
	}
	return c
}

// contacts is whom the user can start a message to: by the last poll the school's recipients for the
// user's role (classes and groups first), before it the current teachers.
func (ec *EduPageClient) contacts() []*bridgev2.ResolveIdentifierResponse {
	var out []*bridgev2.ResolveIdentifierResponse
	if list := ec.school.Load(); list != nil {
		for _, r := range *list {
			out = append(out, ec.recipientContact(r))
		}
		return out
	}
	ec.teachers.Range(func(k, v any) bool {
		out = append(out, ec.contact(k.(networkid.UserID), v.(edupage.Contact).Name))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return *out[i].UserInfo.Name < *out[j].UserInfo.Name })
	return out
}

// GetContactList is whom EduPage lets the user start a message to.
func (ec *EduPageClient) GetContactList(_ context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	return ec.contacts(), nil
}

func fold(s string) string { return strings.ToLower(edupage.Fold(s)) }

func (ec *EduPageClient) SearchUsers(_ context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	q := fold(strings.TrimSpace(query))
	var out []*bridgev2.ResolveIdentifierResponse
	for _, c := range ec.contacts() {
		if q != "" && strings.Contains(fold(*c.UserInfo.Name), q) {
			out = append(out, c)
		}
	}
	return out, nil
}

// ResolveIdentifier takes a ghost ID like ucitel123 (or EduPage's Ucitel123), or recipients of a new
// message: one like StudTrieda7, or several joined by ";".
func (ec *EduPageClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	identifier = strings.TrimSpace(identifier)
	id := makeUserID(identifier, "")
	name := ""
	if v, ok := ec.teachers.Load(id); ok && !strings.Contains(identifier, ";") {
		name = v.(edupage.Contact).Name
	} else if n, ok := ec.names.Load(id); ok && edupageID(id) != "" && !strings.Contains(identifier, ";") {
		name = n.(string)
	} else {
		return ec.resolveKomu(ctx, identifier, createChat)
	}
	resp := ec.contact(id, name)
	if createChat {
		ghost, err := ec.Main.Bridge.GetGhostByID(ctx, id)
		if err != nil {
			return nil, err
		}
		resp.Ghost = ghost
		chat, err := ec.CreateChatWithGhost(ctx, ghost)
		if err != nil {
			return nil, err
		}
		resp.Chat = chat
	}
	return resp, nil
}

func (ec *EduPageClient) resolveKomu(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	list, err := ec.recipients(ctx)
	if err != nil {
		return nil, err
	}
	ids := strings.Split(identifier, ";")
	lower, _, err := pick(list, ids)
	if err != nil {
		return nil, bridgev2.WrapRespErrManual(errNotFound, mautrix.MNotFound.ErrCode, 404)
	}
	resp := ec.contact("", komuName(list, lower))
	if len(lower) == 1 {
		resp.UserID = networkid.UserID(lower[0])
	}
	if createChat {
		if resp.Chat, err = ec.komuChat(ctx, lower, ""); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (ec *EduPageClient) CreateChatWithGhost(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.CreateChatResponse, error) {
	if edupageID(ghost.ID) == "" {
		return nil, bridgev2.WrapRespErrManual(errNotFound, mautrix.MNotFound.ErrCode, 404)
	}
	key := ec.portalKey(personPortal(ghost.ID))
	portal, err := ec.Main.Bridge.GetPortalByKey(ctx, key)
	if err != nil {
		return nil, err
	}
	info, err := ec.GetChatInfo(ctx, portal)
	if err != nil {
		return nil, err
	}
	return &bridgev2.CreateChatResponse{PortalKey: key, PortalInfo: info}, nil
}

type notFound struct{}

func (notFound) Error() string { return "no such person on EduPage" }

var errNotFound = notFound{}
