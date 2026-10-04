/*
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	bootpartition.go: SD card write protection helpers.

	The root filesystem is already protected by the overlay (see init-overlay / overlayctl).
	The FAT boot partition (/boot/firmware) is not: it holds stratux.conf and is mounted rw by
	default. A FAT partition that is yanked from power while dirty is the most common way to
	corrupt a Stratux SD card. stratux-pre-start.sh therefore remounts it read-only, and every
	write we do to it goes through withBootPartitionWritable(), which opens a short rw window,
	syncs, and closes it again.
*/

package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const bootPartitionMountPoint = "/boot/firmware"

var bootPartitionMutex sync.Mutex

// mountOptions returns the mount options of the given mount point, or nil if it is not mounted.
func mountOptions(mountPoint string) []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var opts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[1] == mountPoint {
			opts = strings.Split(fields[3], ",") // last match wins, like the kernel's view
		}
	}
	return opts
}

func isMountReadOnly(mountPoint string) bool {
	for _, o := range mountOptions(mountPoint) {
		if o == "ro" {
			return true
		}
	}
	return false
}

// isOverlayRootActive reports whether / is the tmpfs-backed overlay (i.e. root is write protected).
func isOverlayRootActive() bool {
	_, err := os.Stat("/overlay/robase/overlay")
	return err == nil
}

func remountBootPartition(mode string) error {
	out, err := exec.Command("mount", "-o", "remount,"+mode, bootPartitionMountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remount %s %s: %s (%s)", bootPartitionMountPoint, mode, err.Error(), strings.TrimSpace(string(out)))
	}
	return nil
}

// withBootPartitionWritable runs fn with the boot partition mounted rw. If the partition was read-only
// before, it is synced and remounted read-only afterwards. If it is not mounted, or already rw (dev
// machines, or the user opted out via /boot/firmware/.stratux-boot-rw), fn is simply called.
func withBootPartitionWritable(fn func() error) error {
	bootPartitionMutex.Lock()
	defer bootPartitionMutex.Unlock()

	wasReadOnly := isMountReadOnly(bootPartitionMountPoint)
	if wasReadOnly {
		if err := remountBootPartition("rw"); err != nil {
			return err
		}
	}
	err := fn()
	syscall.Sync()
	if wasReadOnly {
		if rerr := remountBootPartition("ro"); rerr != nil {
			log.Printf("SD card protection: %s\n", rerr.Error())
			if err == nil {
				err = rerr
			}
		}
	}
	return err
}

// writeFileAtomic writes data to a temp file next to path, fsyncs it and renames it over path, so
// a power loss leaves either the old or the new file, never a truncated one.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	fd, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err = fd.Write(data); err == nil {
		err = fd.Sync()
	}
	if cerr := fd.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
