package tailscale

import (
	"fmt"
	"regexp"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"
)

type Config struct{ HostnamePrefix, AuthKey string }

var prefixPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)

func ParseConfig(entries []*pluginv1.ConfigEntry) (Config, error) {
	c := Config{HostnamePrefix: "silo"}
	seen := false
	for _, entry := range entries {
		if entry.GetKey() != "tailscale" || seen {
			return c, fmt.Errorf("unexpected or duplicate configuration section")
		}
		seen = true
		for key, value := range entry.GetValue().GetFields() {
			if _, ok := value.GetKind().(*structpb.Value_StringValue); !ok {
				return c, fmt.Errorf("configuration fields must be strings")
			}
			switch key {
			case "hostname_prefix":
				c.HostnamePrefix = value.GetStringValue()
			case "auth_key":
				c.AuthKey = value.GetStringValue()
			default:
				return c, fmt.Errorf("unexpected configuration field")
			}
		}
	}
	if !prefixPattern.MatchString(c.HostnamePrefix) {
		return c, fmt.Errorf("hostname prefix must be 1–32 lowercase letters, digits or internal hyphens")
	}
	if c.AuthKey != "" && (!strings.HasPrefix(c.AuthKey, "tskey-auth-") || strings.ContainsAny(c.AuthKey, " \r\n\t")) {
		return c, fmt.Errorf("auth key must be a Tailscale auth key")
	}
	return c, nil
}

func Hostname(c Config, info *runtimehost.HostInfo) string {
	if info.HostRole == runtimehost.HostRoleProxy {
		return fmt.Sprintf("%s-proxy-%d", c.HostnamePrefix, info.NodeID)
	}
	return c.HostnamePrefix + "-api"
}
