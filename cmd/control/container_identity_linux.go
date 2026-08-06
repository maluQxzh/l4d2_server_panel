//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

func prepareContainerRuntimeIdentity() error {
	socket := os.Getenv("CONTAINER_DOCKER_SOCKET")
	if socket == "" || os.Geteuid() != 0 {
		return nil
	}
	if socket != "/var/run/docker.sock" {
		return errors.New("container Docker Socket must use /var/run/docker.sock")
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return fmt.Errorf("inspect Docker Socket: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return errors.New("container Docker endpoint must be a socket and not a symlink")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("read Docker Socket ownership")
	}
	panel, err := user.Lookup("panel")
	if err != nil {
		return fmt.Errorf("resolve panel user: %w", err)
	}
	uid, err := strconv.Atoi(panel.Uid)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(panel.Gid)
	if err != nil {
		return err
	}
	for _, directory := range []string{"/app/data", "/app/spool", "/app/updates"} {
		if err := prepareContainerDirectory(directory, uid, gid); err != nil {
			return err
		}
	}
	groups := []int{gid}
	if int(stat.Gid) != gid {
		groups = append(groups, int(stat.Gid))
	}
	if err := syscall.Setgroups(groups); err != nil {
		return fmt.Errorf("adopt Docker Socket group: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("drop container group identity: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("drop container user identity: %w", err)
	}
	syscall.Umask(0o077)
	return nil
}

func prepareContainerDirectory(path string, uid, gid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect container directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("container path %s must be a directory and not a symlink", path)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		return fmt.Errorf("set container directory ownership %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set container directory permissions %s: %w", path, err)
	}
	return nil
}
