package agent

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func TestRCONPacketRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	want := rconPacket{ID: 42, Type: rconExecCommand, Body: "status"}
	if err := writeRCONPacket(&buffer, want); err != nil {
		t.Fatal(err)
	}
	got, err := readRCONPacket(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("packet = %#v, want %#v", got, want)
	}
}

func TestParseA2SPlayers(t *testing.T) {
	data := []byte{0xff, 0xff, 0xff, 0xff, 0x44, 1, 0}
	data = append(data, []byte("Coach\x00")...)
	var score [4]byte
	binary.LittleEndian.PutUint32(score[:], uint32(12))
	data = append(data, score[:]...)
	var duration [4]byte
	binary.LittleEndian.PutUint32(duration[:], math.Float32bits(45.5))
	data = append(data, duration[:]...)
	players, err := parseA2SPlayers(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 1 || players[0].Name != "Coach" || players[0].Score != 12 {
		t.Fatalf("players = %#v", players)
	}
}

func TestVPKHeaderValidation(t *testing.T) {
	path := t.TempDir() + "/addon.vpk"
	if err := os.WriteFile(path, validTestVPK([]byte("header-test"), 2), 0o600); err != nil {
		t.Fatal(err)
	}
	version, _, err := validateVPK(path)
	if err != nil || version != 2 {
		t.Fatalf("version=%d err=%v", version, err)
	}
}

func TestParseRCONStatusPlayersOmitsAddresses(t *testing.T) {
	status := "# userid name uniqueid connected ping loss state rate adr\n# 2 17 \"Zoey\" STEAM_1:1:12345 01:20 42 0 active 30000 203.0.113.8:27005\n# 3 18 \"Coach Bot\" BOT 00:10 0 0 active 30000 127.0.0.1:0\n"
	players := parseRCONStatusPlayers(status)
	if len(players) != 2 {
		t.Fatalf("players = %#v", players)
	}
	if players[0].SteamID != "STEAM_1:1:12345" || players[0].UserID != 17 || players[0].Ping != 42 {
		t.Fatalf("human = %#v", players[0])
	}
	if !players[1].Bot || players[1].SteamID != "" {
		t.Fatalf("bot = %#v", players[1])
	}
}

func TestParseRCONStatusPlayersAcceptsShortL4D2Rows(t *testing.T) {
	status := "# userid name uniqueid connected ping loss state\r\n" +
		"# 17 \"中文 玩家\" STEAM_1:1:12345 01:20 42 0 active\r\n" +
		"# 18 \"Coach\" BOT active\r\n" +
		"# 3 19 \"Another\" [U:1:789] 00:15 23 0 active 30000\r\n"
	players := parseRCONStatusPlayers(status)
	if len(players) != 3 || players[0].UserID != 17 || players[0].Ping != 42 || players[0].Name != "中文 玩家" || !players[1].Bot || players[2].UserID != 19 {
		t.Fatalf("short status rows = %#v", players)
	}
}
