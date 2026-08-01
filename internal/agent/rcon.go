package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	rconResponseValue = 0
	rconExecCommand   = 2
	rconAuth          = 3
	rconMaxPacket     = 4 << 20
	rconMaxResponse   = 16 << 20
)

type rconPacket struct {
	ID   int32
	Type int32
	Body string
}

type RCONClient struct{ Timeout time.Duration }

func (c RCONClient) Execute(ctx context.Context, address, password, command string) (string, error) {
	if password == "" {
		return "", errors.New("RCON password is required")
	}
	if command == "" || strings.ContainsRune(command, '\x00') || strings.ContainsAny(command, "\r\n") {
		return "", errors.New("invalid RCON command")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("connect RCON: %w", err)
	}
	defer conn.Close()
	setConnDeadline(conn, ctx, timeout)
	authID := randomPacketID()
	if err := writeRCONPacket(conn, rconPacket{ID: authID, Type: rconAuth, Body: password}); err != nil {
		return "", err
	}
	authed := false
	for i := 0; i < 4; i++ {
		packet, err := readRCONPacket(conn)
		if err != nil {
			return "", fmt.Errorf("read RCON authentication: %w", err)
		}
		if packet.ID == -1 {
			return "", errors.New("RCON authentication failed")
		}
		if packet.Type == rconExecCommand && packet.ID == authID {
			authed = true
			break
		}
	}
	if !authed {
		return "", errors.New("RCON server did not confirm authentication")
	}
	requestID := randomPacketID()
	sentinelID := requestID + 1
	if sentinelID <= 0 {
		sentinelID = 1
	}
	if err := writeRCONPacket(conn, rconPacket{ID: requestID, Type: rconExecCommand, Body: command}); err != nil {
		return "", err
	}
	if err := writeRCONPacket(conn, rconPacket{ID: sentinelID, Type: rconResponseValue, Body: ""}); err != nil {
		return "", err
	}
	var result strings.Builder
	for {
		packet, err := readRCONPacket(conn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() && result.Len() > 0 {
				return result.String(), nil
			}
			return "", fmt.Errorf("read RCON response: %w", err)
		}
		if packet.ID == sentinelID {
			break
		}
		if packet.ID != requestID {
			continue
		}
		if result.Len()+len(packet.Body) > rconMaxResponse {
			return "", errors.New("RCON response exceeds size limit")
		}
		result.WriteString(packet.Body)
	}
	return result.String(), nil
}

func randomPacketID() int32 {
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	id := int32(binary.LittleEndian.Uint32(raw[:]) & 0x3fffffff)
	if id == 0 {
		id = 1
	}
	return id
}

func writeRCONPacket(w io.Writer, packet rconPacket) error {
	if strings.ContainsRune(packet.Body, '\x00') {
		return errors.New("RCON body contains NUL")
	}
	length := 4 + 4 + len(packet.Body) + 2
	if length > rconMaxPacket {
		return errors.New("RCON packet exceeds size limit")
	}
	buf := bytes.NewBuffer(make([]byte, 0, length+4))
	_ = binary.Write(buf, binary.LittleEndian, int32(length))
	_ = binary.Write(buf, binary.LittleEndian, packet.ID)
	_ = binary.Write(buf, binary.LittleEndian, packet.Type)
	buf.WriteString(packet.Body)
	buf.WriteByte(0)
	buf.WriteByte(0)
	_, err := w.Write(buf.Bytes())
	return err
}

func readRCONPacket(r io.Reader) (rconPacket, error) {
	var length int32
	if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
		return rconPacket{}, err
	}
	if length < 10 || length > rconMaxPacket {
		return rconPacket{}, fmt.Errorf("invalid RCON packet length %d", length)
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(r, body); err != nil {
		return rconPacket{}, err
	}
	id := int32(binary.LittleEndian.Uint32(body[0:4]))
	typ := int32(binary.LittleEndian.Uint32(body[4:8]))
	if body[len(body)-1] != 0 || body[len(body)-2] != 0 {
		return rconPacket{}, errors.New("RCON packet is not NUL terminated")
	}
	return rconPacket{ID: id, Type: typ, Body: string(body[8 : len(body)-2])}, nil
}

func setConnDeadline(conn net.Conn, ctx context.Context, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
}
