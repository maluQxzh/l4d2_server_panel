package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"time"
)

var a2sHeader = []byte{0xff, 0xff, 0xff, 0xff}

type A2SInfo struct {
	Protocol   byte   `json:"protocol"`
	Name       string `json:"name"`
	Map        string `json:"map"`
	Folder     string `json:"folder"`
	Game       string `json:"game"`
	AppID      uint16 `json:"app_id"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"max_players"`
	Bots       int    `json:"bots"`
	Password   bool   `json:"password"`
	VAC        bool   `json:"vac"`
	Version    string `json:"version"`
}

type A2SPlayer struct {
	Index    byte    `json:"index"`
	Name     string  `json:"name"`
	Score    int32   `json:"score"`
	Duration float32 `json:"duration"`
}

type A2SClient struct{ Timeout time.Duration }

func (c A2SClient) Info(ctx context.Context, address string) (A2SInfo, error) {
	request := append(append([]byte{}, a2sHeader...), 0x54)
	request = append(request, []byte("Source Engine Query\x00")...)
	response, err := c.query(ctx, address, request, false)
	if err != nil {
		return A2SInfo{}, err
	}
	return parseA2SInfo(response)
}

func (c A2SClient) Players(ctx context.Context, address string) ([]A2SPlayer, error) {
	request := append(append([]byte{}, a2sHeader...), 0x55, 0xff, 0xff, 0xff, 0xff)
	response, err := c.query(ctx, address, request, true)
	if err != nil {
		return nil, err
	}
	return parseA2SPlayers(response)
}

func (c A2SClient) query(ctx context.Context, address string, request []byte, replaceChallenge bool) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	setConnDeadline(conn, ctx, timeout)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := conn.Write(request); err != nil {
			return nil, err
		}
		buf := make([]byte, 64*1024)
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		response := buf[:n]
		if len(response) < 5 {
			return nil, errors.New("short A2S response")
		}
		if bytes.Equal(response[:4], []byte{0xfe, 0xff, 0xff, 0xff}) {
			return nil, errors.New("split or compressed A2S responses are not supported")
		}
		if !bytes.Equal(response[:4], a2sHeader) {
			return nil, errors.New("invalid A2S response header")
		}
		if response[4] != 0x41 {
			return response, nil
		}
		if len(response) < 9 || attempt != 0 {
			return nil, errors.New("invalid A2S challenge response")
		}
		challenge := response[5:9]
		if replaceChallenge {
			copy(request[len(request)-4:], challenge)
		} else {
			request = append(request, challenge...)
		}
	}
	return nil, errors.New("A2S challenge failed")
}

func parseA2SInfo(data []byte) (A2SInfo, error) {
	if len(data) < 6 || data[4] != 0x49 {
		return A2SInfo{}, errors.New("unexpected A2S_INFO response")
	}
	r := &packetReader{data: data[5:]}
	info := A2SInfo{}
	var err error
	if info.Protocol, err = r.byte(); err != nil {
		return info, err
	}
	if info.Name, err = r.cstring(); err != nil {
		return info, err
	}
	if info.Map, err = r.cstring(); err != nil {
		return info, err
	}
	if info.Folder, err = r.cstring(); err != nil {
		return info, err
	}
	if info.Game, err = r.cstring(); err != nil {
		return info, err
	}
	if info.AppID, err = r.uint16(); err != nil {
		return info, err
	}
	players, err := r.byte()
	if err != nil {
		return info, err
	}
	info.Players = int(players)
	maxPlayers, err := r.byte()
	if err != nil {
		return info, err
	}
	info.MaxPlayers = int(maxPlayers)
	bots, err := r.byte()
	if err != nil {
		return info, err
	}
	info.Bots = int(bots)
	if _, err = r.byte(); err != nil {
		return info, err
	} // dedicated/listen/proxy
	if _, err = r.byte(); err != nil {
		return info, err
	} // environment
	password, err := r.byte()
	if err != nil {
		return info, err
	}
	info.Password = password != 0
	vac, err := r.byte()
	if err != nil {
		return info, err
	}
	info.VAC = vac != 0
	if info.Version, err = r.cstring(); err != nil {
		return info, err
	}
	return info, nil
}

func parseA2SPlayers(data []byte) ([]A2SPlayer, error) {
	if len(data) < 6 || data[4] != 0x44 {
		return nil, errors.New("unexpected A2S_PLAYER response")
	}
	r := &packetReader{data: data[5:]}
	count, err := r.byte()
	if err != nil {
		return nil, err
	}
	players := make([]A2SPlayer, 0, int(count))
	for i := 0; i < int(count); i++ {
		var p A2SPlayer
		if p.Index, err = r.byte(); err != nil {
			return nil, err
		}
		if p.Name, err = r.cstring(); err != nil {
			return nil, err
		}
		if p.Score, err = r.int32(); err != nil {
			return nil, err
		}
		bits, err := r.uint32()
		if err != nil {
			return nil, err
		}
		p.Duration = math.Float32frombits(bits)
		players = append(players, p)
	}
	return players, nil
}

type packetReader struct {
	data []byte
	off  int
}

func (r *packetReader) byte() (byte, error) {
	if r.off >= len(r.data) {
		return 0, fmt.Errorf("truncated packet at %d", r.off)
	}
	b := r.data[r.off]
	r.off++
	return b, nil
}
func (r *packetReader) cstring() (string, error) {
	for i := r.off; i < len(r.data); i++ {
		if r.data[i] == 0 {
			s := string(r.data[r.off:i])
			r.off = i + 1
			return s, nil
		}
	}
	return "", errors.New("unterminated packet string")
}
func (r *packetReader) uint16() (uint16, error) {
	if r.off+2 > len(r.data) {
		return 0, errors.New("truncated uint16")
	}
	v := binary.LittleEndian.Uint16(r.data[r.off:])
	r.off += 2
	return v, nil
}
func (r *packetReader) uint32() (uint32, error) {
	if r.off+4 > len(r.data) {
		return 0, errors.New("truncated uint32")
	}
	v := binary.LittleEndian.Uint32(r.data[r.off:])
	r.off += 4
	return v, nil
}
func (r *packetReader) int32() (int32, error) { v, err := r.uint32(); return int32(v), err }
