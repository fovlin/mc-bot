package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveStatus pings a real Minecraft server and is skipped unless
// MCBOT_LIVE=1 is set. It verifies the modern status protocol against the
// example server used by this bot.
func TestLiveStatus(t *testing.T) {
	if os.Getenv("MCBOT_LIVE") == "" {
		t.Skip("set MCBOT_LIVE=1 to run the live server test")
	}

	st := pingJava(context.Background(), "mc.acovia.net", 8*time.Second)
	if !st.Online {
		t.Fatalf("mc.acovia.net is offline: %s", st.Err)
	}
	if st.Version == "" || st.Version == "unknown" {
		t.Fatalf("version is empty: %+v", st)
	}
	if st.MOTD == "" {
		t.Fatalf("MOTD is empty: %+v", st)
	}
	if st.MaxPlayers <= 0 {
		t.Fatalf("invalid max player count: %+v", st)
	}
	t.Logf("online %d/%d, version %s, latency %dms, MOTD %q, sample players %v",
		st.OnlinePlayers, st.MaxPlayers, st.Version, st.Latency.Milliseconds(), st.MOTD, st.Sample)
}
