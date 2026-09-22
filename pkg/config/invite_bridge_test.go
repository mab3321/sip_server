package config

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestInviteBridgeTransferConfig(t *testing.T) {
	var conf Config
	require.NoError(t, yaml.Unmarshal([]byte("invite_bridge_transfer: true\ninvite_direct_media_transfer: true\n"), &conf))
	require.True(t, conf.InviteBridgeTransfer)
	require.True(t, conf.InviteDirectMediaTransfer)
}
