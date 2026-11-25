package connector

import (
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
)

func (ec *EduPageConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		Provisioning: bridgev2.ProvisioningCapabilities{
			ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{CreateDM: true, Search: true},
		},
	}
}

// roomFeatures: EduPage messages are plain text with files; a message cannot be edited, deleted or
// reacted to from outside edupage.org, and a reply always goes to the thread of the message it answers.
func roomFeatures(maxSize int64) *event.RoomFeatures {
	file := &event.FileFeatures{
		MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
		Caption:   event.CapLevelFullySupported,
		MaxSize:   maxSize,
	}
	return &event.RoomFeatures{
		ID: "io.github.gooder24.edupage.capabilities.2026_11_25",
		File: event.FileFeatureMap{
			event.MsgImage: file,
			event.MsgVideo: file,
			event.MsgAudio: file,
			event.MsgFile:  file,
		},
		Reply:               event.CapLevelFullySupported,
		Edit:                event.CapLevelRejected,
		Delete:              event.CapLevelRejected,
		Reaction:            event.CapLevelRejected,
		ReadReceipts:        false,
		TypingNotifications: false,
	}
}

// noticeFeatures: the "EduPage" room of notices takes nothing; EduPage has no way to answer a notice.
func noticeFeatures() *event.RoomFeatures {
	return &event.RoomFeatures{
		ID:       "io.github.gooder24.edupage.capabilities.notices.2026_11_25",
		Reply:    event.CapLevelRejected,
		Edit:     event.CapLevelRejected,
		Delete:   event.CapLevelRejected,
		Reaction: event.CapLevelRejected,
	}
}
