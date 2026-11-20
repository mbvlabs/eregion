//go:build !linux

package controlplane

func currentWorkerIncarnation() (string, bool) { return "", false }

func workerIncarnationDead(string) bool { return false }
