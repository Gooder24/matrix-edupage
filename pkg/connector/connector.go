package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

type EduPageConnector struct {
	Bridge *bridgev2.Bridge
	Config Config
}

var _ bridgev2.NetworkConnector = (*EduPageConnector)(nil)

func (ec *EduPageConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:          "EduPage",
		NetworkURL:           "https://www.edupage.org",
		NetworkID:            "edupage",
		BeeperBridgeType:     "github.com/Gooder24/matrix-edupage",
		DefaultPort:          29341,
		DefaultCommandPrefix: "!edupage",
	}
}

func (ec *EduPageConnector) Init(bridge *bridgev2.Bridge) { ec.Bridge = bridge }

func (ec *EduPageConnector) Start(ctx context.Context) error {
	if ec.Config.PollInterval <= 0 {
		ec.Config.PollInterval = 5 * time.Minute
	}
	if ec.Config.RecentDays <= 0 {
		ec.Config.RecentDays = 7
	}
	if ec.Config.MaxFileSize <= 0 {
		ec.Config.MaxFileSize = 25 << 20
	}
	return nil
}

func (ec *EduPageConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Message:   func() any { return &MessageMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (ec *EduPageConnector) GetBridgeInfoVersion() (info, caps int) { return 1, 1 }

func (ec *EduPageConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	login.Client = &EduPageClient{Main: ec, UserLogin: login}
	return nil
}
