//go:build !linux && !windows

package main

func prepareServiceConfigOwnership(string) error { return nil }
