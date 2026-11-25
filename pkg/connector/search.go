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

func (ec *EduPageClient) contacts() []*bridgev2.ResolveIdentifierResponse {
	var out []*bridgev2.ResolveIdentifierResponse
	ec.teachers.Range(func(k, v any) bool {
		out = append(out, ec.contact(k.(networkid.UserID), v.(edupage.Contact).Name))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return *out[i].UserInfo.Name < *out[j].UserInfo.Name })
	return out
}

// GetContactList is the school's current teachers, the people EduPage lets a pupil start a message to.
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

// ResolveIdentifier takes a ghost ID like ucitel123 (or EduPage's Ucitel123).
func (ec *EduPageClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	id := makeUserID(strings.TrimSpace(identifier), "")
	name := ""
	if v, ok := ec.teachers.Load(id); ok {
		name = v.(edupage.Contact).Name
	} else if n, ok := ec.names.Load(id); ok && edupageID(id) != "" {
		name = n.(string)
	} else {
		return nil, bridgev2.WrapRespErrManual(errNotFound, mautrix.MNotFound.ErrCode, 404)
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
