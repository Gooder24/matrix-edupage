package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/Gooder24/matrix-edupage/pkg/edupage"
)

// ErrNoRecipient: outside a person's room EduPage needs a message to answer.
var ErrNoRecipient = bridgev2.WrapErrorInStatus(errors.New("EduPage needs a recipient: reply to a message, or write in a person's chat")).
	WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true).
	WithStatus(event.MessageStatusFail).WithErrorReason(event.MessageStatusUnsupported)

type upload struct {
	name, mimeType string
	data           []byte
}

func (ec *EduPageClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	content := msg.Content
	text := content.Body
	var files []upload
	switch content.MsgType {
	case event.MsgText, event.MsgNotice:
	case event.MsgEmote:
		text = "* " + text
	case event.MsgImage, event.MsgFile, event.MsgVideo, event.MsgAudio:
		f, err := ec.download(ctx, content)
		if err != nil {
			return nil, err
		}
		files = append(files, *f)
		text = ""
		if content.FileName != "" && content.Body != content.FileName {
			text = content.Body
		}
	default:
		return nil, fmt.Errorf("%w %s", bridgev2.ErrUnsupportedMessageType, content.MsgType)
	}
	var root int64
	if msg.ReplyTo != nil {
		meta, _ := msg.ReplyTo.Metadata.(*MessageMetadata)
		if meta == nil || meta.Sig != "" {
			return nil, ErrNoRecipient // a notice is no thread to answer in
		}
		root = meta.Root
		if root == 0 {
			root = parseMessageID(msg.ReplyTo.ID)
		}
	}
	person, isPerson := portalPerson(msg.Portal.ID)
	to := edupageID(person)
	if root == 0 && (!isPerson || to == "") {
		return nil, ErrNoRecipient
	}
	var id int64
	err := ec.withSession(ctx, func(c *edupage.Client) error {
		// Uploaded with the session that sends, so a renewed session uploads them again.
		atts := map[string]string{}
		for _, f := range files {
			path, name, err := c.Upload(ctx, f.name, f.mimeType, f.data)
			if err != nil {
				return err
			}
			atts[path] = name
		}
		var err error
		if root != 0 {
			id, err = c.SendReply(ctx, root, text, atts)
		} else {
			id, err = c.SendMessage(ctx, to, text, atts)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if root == 0 {
		root = id
	}
	return &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID:        makeMessageID(id),
		SenderID:  ec.ownID(),
		Timestamp: time.Now(),
		Metadata:  &MessageMetadata{Root: root},
	}, StreamOrder: id}, nil
}

func (ec *EduPageClient) download(ctx context.Context, content *event.MessageEventContent) (*upload, error) {
	name := content.FileName
	if name == "" {
		name = content.Body
	}
	if name == "" {
		name = "subor"
	}
	kind := "application/octet-stream"
	if content.Info != nil && content.Info.MimeType != "" {
		kind = content.Info.MimeType
	}
	uri := content.URL
	if content.File != nil {
		uri = content.File.URL
	}
	data, err := ec.Main.Bridge.Bot.DownloadMedia(ctx, uri, content.File)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", bridgev2.ErrMediaDownloadFailed, err)
	}
	if int64(len(data)) > ec.Main.Config.MaxFileSize {
		return nil, fmt.Errorf("%w: the file is larger than %d MB", bridgev2.ErrMediaTooLarge, ec.Main.Config.MaxFileSize>>20)
	}
	return &upload{name: name, mimeType: kind, data: data}, nil
}
