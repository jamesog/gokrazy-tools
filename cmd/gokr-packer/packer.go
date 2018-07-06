// gokr-packer compiles and installs the specified Go packages as well
// as the gokrazy Go packages and packs them into an SD card image for
// the Raspberry Pi 3.
package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gokrazy/internal/fat"
	"github.com/gokrazy/internal/mbr"
	"github.com/gokrazy/internal/updater"

	// Imported so that the go tool will download the repositories
	_ "github.com/gokrazy/firmware"
	_ "github.com/gokrazy/gokrazy/empty"
	_ "github.com/gokrazy/kernel"
)

const MB = 1024 * 1024

var (
	overwrite = flag.String("overwrite",
		"",
		"Destination device (e.g. /dev/sdb) or file (e.g. /tmp/gokrazy.img) to overwrite with a full disk image")

	overwriteBoot = flag.String("overwrite_boot",
		"",
		"Destination partition (e.g. /dev/sdb1) or file (e.g. /tmp/boot.fat) to overwrite with the boot file system")

	overwriteRoot = flag.String("overwrite_root",
		"",
		"Destination partition (e.g. /dev/sdb2) or file (e.g. /tmp/root.fat) to overwrite with the root file system")

	overwriteInit = flag.String("overwrite_init",
		"",
		"Destination file (e.g. /tmp/init.go) to overwrite with the generated init source code")

	targetStorageBytes = flag.Int("target_storage_bytes",
		0,
		"Number of bytes which the target storage device (SD card) has. Required for using -overwrite=<file>")

	initPkg = flag.String("init_pkg",
		"",
		"Go package to install as /gokrazy/init instead of the auto-generated one")

	update = flag.String("update",
		os.Getenv("GOKRAZY_UPDATE"),
		`URL of a gokrazy installation (e.g. http://gokrazy:mypassword@myhostname/) to update. The special value "yes" uses the stored password and -hostname value to construct the URL`)

	hostname = flag.String("hostname",
		"gokrazy",
		"host name to set on the target system. Will be sent when acquiring DHCP leases")

	gokrazyPkgList = flag.String("gokrazy_pkgs",
		"github.com/gokrazy/gokrazy/cmd/...",
		"comma-separated list of packages installed to /gokrazy/ (boot and system utilities)")
)

var gokrazyPkgs []string

func findCACerts() (string, error) {
	home, err := homedir()
	if err != nil {
		return "", err
	}
	certFiles = append(certFiles, filepath.Join(home, ".config", "gokrazy", "cacert.pem"))
	for _, fn := range certFiles {
		if _, err := os.Stat(fn); err == nil {
			return fn, nil
		}
	}
	return "", fmt.Errorf("did not find any of: %s", strings.Join(certFiles, ", "))
}

type countingWriter int64

func (cw *countingWriter) Write(p []byte) (n int, err error) {
	*cw += countingWriter(len(p))
	return len(p), nil
}

func writeBootFile(filename string) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := writeBoot(f); err != nil {
		return err
	}
	return f.Close()
}

func writeRootFile(filename string, root *fileInfo) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := writeRoot(f, root); err != nil {
		return err
	}
	return f.Close()
}

func partitionPath(base, num string) string {
	if strings.HasPrefix(base, "/dev/mmcblk") {
		return base + "p" + num
	} else if strings.HasPrefix(base, "/dev/disk") ||
		strings.HasPrefix(base, "/dev/rdisk") {
		return base + "s" + num
	}
	return base + num
}

func writeMBRFile(filename string) error {
	f, err := os.OpenFile(filename, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := writeMBR(f); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

func overwriteDevice(dev string, root *fileInfo) error {
	log.Printf("partitioning %s", dev)

	if err := partition(*overwrite); err != nil {
		return err
	}

	// TODO: get rid of this ridiculous sleep. Without it, I get -EACCES when
	// trying to open /dev/sdb1.
	log.Printf("waiting for %s to appear", partitionPath(dev, "1"))
	time.Sleep(1 * time.Second)

	if err := writeBootFile(partitionPath(dev, "1")); err != nil {
		return err
	}

	if err := writeMBRFile(*overwrite); err != nil {
		return err
	}

	if err := writeRootFile(partitionPath(dev, "2"), root); err != nil {
		return err
	}

	fmt.Printf("If your applications need to store persistent data, create a file system using e.g.:\n")
	fmt.Printf("\n")
	fmt.Printf("\tmkfs.ext4 %s\n", partitionPath(dev, "4"))
	fmt.Printf("\n")

	return nil
}

type offsetReadSeeker struct {
	io.ReadSeeker
	offset int64
}

func (ors *offsetReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekStart {
		// github.com/gokrazy/internal/fat.Reader only uses io.SeekStart
		return ors.ReadSeeker.Seek(offset+ors.offset, io.SeekStart)
	}
	return ors.ReadSeeker.Seek(offset, whence)
}

func writeMBR(f io.ReadWriteSeeker) error {
	rd, err := fat.NewReader(&offsetReadSeeker{f, 8192 * 512})
	if err != nil {
		return err
	}
	vmlinuzOffset, _, err := rd.Extents("/vmlinuz")
	if err != nil {
		return err
	}
	cmdlineOffset, _, err := rd.Extents("/cmdline.txt")
	if err != nil {
		return err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	vmlinuzLba := uint32((vmlinuzOffset / 512) + 8192)
	cmdlineTxtLba := uint32((cmdlineOffset / 512) + 8192)

	mbr := mbr.Configure(vmlinuzLba, cmdlineTxtLba)
	if _, err := f.Write(mbr[:]); err != nil {
		return err
	}

	return nil
}

func overwriteFile(filename string, root *fileInfo) (bootSize int64, rootSize int64, err error) {
	f, err := os.Create(*overwrite)
	if err != nil {
		return 0, 0, err
	}

	if err := f.Truncate(int64(*targetStorageBytes)); err != nil {
		return 0, 0, err
	}

	if err := writePartitionTable(f, uint64(*targetStorageBytes)); err != nil {
		return 0, 0, err
	}

	if _, err := f.Seek(8192*512, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var bs countingWriter
	if err := writeBoot(io.MultiWriter(f, &bs)); err != nil {
		return 0, 0, err
	}

	if err := writeMBR(f); err != nil {
		return 0, 0, err
	}

	if _, err := f.Seek(8192*512+100*MB, io.SeekStart); err != nil {
		return 0, 0, err
	}

	tmp, err := ioutil.TempFile("", "gokr-packer")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := writeRoot(tmp, root); err != nil {
		return 0, 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}

	var rs countingWriter
	if _, err := io.Copy(io.MultiWriter(f, &rs), tmp); err != nil {
		return 0, 0, err
	}

	return int64(bs), int64(rs), f.Close()
}

const usage = `
gokr-packer packs gokrazy installations into SD card or file system images.

Usage:
To directly partition and overwrite an SD card:
gokr-packer -overwrite=<device> <go-package> [<go-package>…]

To create an SD card image on the file system:
gokr-packer -overwrite=<file> -target_storage_bytes=<bytes> <go-package> [<go-package>…]

To create a file system image of the boot or root file system:
gokr-packer [-overwrite_boot=<file>|-overwrite_root=<file>] <go-package> [<go-package>…]

To create file system images of both file systems:
gokr-packer -overwrite_boot=<file> -overwrite_root=<file> <go-package> [<go-package>…]

All of the above commands can be combined with the -update flag.

To dump the auto-generated init source code (for use with -init_pkg later):
gokr-packer -overwrite_init=<file> <go-package> [<go-package>…]

Flags:
`

func logic() error {
	cacerts, err := findCACerts()
	if err != nil {
		return err
	}

	log.Printf("installing %v", flag.Args())

	if err := install(); err != nil {
		return err
	}

	root, err := findBins()
	if err != nil {
		return err
	}

	if *initPkg == "" {
		if *overwriteInit != "" {
			return dumpInit(*overwriteInit, root)
		}

		tmpdir, err := buildInit(root)
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpdir)

		gokrazy := root.mustFindDirent("gokrazy")
		gokrazy.dirents = append(gokrazy.dirents, &fileInfo{
			filename: "init",
			fromHost: filepath.Join(tmpdir, "init"),
		})
	}

	pw, pwPath, err := ensurePasswordFileExists()
	if err != nil {
		return err
	}

	for _, dir := range []string{"dev", "etc", "proc", "sys", "tmp", "perm"} {
		root.dirents = append(root.dirents, &fileInfo{
			filename: dir,
		})
	}

	etc := root.mustFindDirent("etc")
	etc.dirents = append(etc.dirents, &fileInfo{
		filename: "localtime",
		fromHost: "/etc/localtime",
	})
	etc.dirents = append(etc.dirents, &fileInfo{
		filename:    "resolv.conf",
		symlinkDest: "/tmp/resolv.conf",
	})
	etc.dirents = append(etc.dirents, &fileInfo{
		filename: "hosts",
		fromLiteral: `127.0.0.1 localhost
::1 localhost
`,
	})
	etc.dirents = append(etc.dirents, &fileInfo{
		filename:    "hostname",
		fromLiteral: *hostname,
	})

	ssl := &fileInfo{filename: "ssl"}
	ssl.dirents = append(ssl.dirents, &fileInfo{
		filename: "ca-bundle.pem",
		fromHost: cacerts,
	})
	etc.dirents = append(etc.dirents, ssl)

	etc.dirents = append(etc.dirents, &fileInfo{
		filename: "gokr-pw.txt",
		fromHost: pwPath,
	})

	// Determine where to write the boot and root images to.
	var (
		isDev              bool
		tmpBoot, tmpRoot   *os.File
		bootSize, rootSize int64
	)
	switch {
	case *overwrite != "":
		st, err := os.Lstat(*overwrite)
		if err != nil && !os.IsNotExist(err) {
			return err
		}

		isDev := err == nil && st.Mode()&os.ModeDevice == os.ModeDevice

		if isDev {
			if err := overwriteDevice(*overwrite, root); err != nil {
				return err
			}
			fmt.Printf("To boot gokrazy, plug the SD card into a Raspberry Pi 3 (no other model supported)\n")
			fmt.Printf("\n")
		} else {
			if *targetStorageBytes == 0 {
				return fmt.Errorf("-target_storage_bytes is required when using -overwrite with a file")
			}
			if *targetStorageBytes%512 != 0 {
				return fmt.Errorf("-target_storage_bytes must be a multiple of 512 (sector size)")
			}
			if lower := 1100*MB + 8192; *targetStorageBytes < lower {
				return fmt.Errorf("-target_storage_bytes must be at least %d (for boot + 2 root file systems)", lower)
			}

			bootSize, rootSize, err = overwriteFile(*overwrite, root)
			if err != nil {
				return err
			}

			fmt.Printf("To boot gokrazy, copy %s to an SD card and plug it into a Raspberry Pi 3 (no other model supported)\n", *overwrite)
			fmt.Printf("\n")
		}

	default:
		if *overwriteBoot != "" {
			if err := writeBootFile(*overwriteBoot); err != nil {
				return err
			}
		}

		if *overwriteRoot != "" {
			if err := writeRootFile(*overwriteRoot, root); err != nil {
				return err
			}
		}

		if *overwriteBoot == "" && *overwriteRoot == "" {
			tmpBoot, err = ioutil.TempFile("", "gokrazy")
			if err != nil {
				return err
			}
			defer os.Remove(tmpBoot.Name())

			if err := writeBoot(tmpBoot); err != nil {
				return err
			}

			tmpRoot, err = ioutil.TempFile("", "gokrazy")
			if err != nil {
				return err
			}
			defer os.Remove(tmpRoot.Name())

			if err := writeRoot(tmpRoot, root); err != nil {
				return err
			}
		}
	}

	fmt.Printf("To interact with the device, gokrazy provides a web interface reachable at:\n")
	fmt.Printf("\n")
	fmt.Printf("\thttp://gokrazy:%s@%s/\n", pw, *hostname)
	fmt.Printf("\n")
	fmt.Printf("There will not be any other output (no HDMI, no serial console, etc.)\n")

	if *update == "" {
		return nil
	}

	// Determine where to read the boot and root images from.
	var rootReader, bootReader io.Reader
	switch {
	case *overwrite != "":
		if isDev {
			bootFile, err := os.Open(*overwrite + "1")
			if err != nil {
				return err
			}
			bootReader = bootFile
			rootFile, err := os.Open(*overwrite + "2")
			if err != nil {
				return err
			}
			rootReader = rootFile
		} else {
			bootFile, err := os.Open(*overwrite)
			if err != nil {
				return err
			}
			if _, err := bootFile.Seek(8192*512, io.SeekStart); err != nil {
				return err
			}
			bootReader = &io.LimitedReader{
				R: bootFile,
				N: rootSize,
			}

			rootFile, err := os.Open(*overwrite)
			if err != nil {
				return err
			}
			if _, err := rootFile.Seek(8192*512+100*MB, io.SeekStart); err != nil {
				return err
			}
			rootReader = &io.LimitedReader{
				R: rootFile,
				N: bootSize,
			}
		}

	default:
		if *overwriteBoot != "" {
			bootFile, err := os.Open(*overwriteBoot)
			if err != nil {
				return err
			}
			bootReader = bootFile
		}

		if *overwriteRoot != "" {
			rootFile, err := os.Open(*overwriteRoot)
			if err != nil {
				return err
			}
			rootReader = rootFile
		}

		if *overwriteBoot == "" && *overwriteRoot == "" {
			if _, err := tmpBoot.Seek(0, io.SeekStart); err != nil {
				return err
			}
			bootReader = tmpBoot

			if _, err := tmpRoot.Seek(0, io.SeekStart); err != nil {
				return err
			}
			rootReader = tmpRoot
		}
	}

	if *update == "yes" {
		*update = "http://gokrazy:" + pw + "@" + *hostname + "/"
	}

	baseUrl, err := url.Parse(*update)
	if err != nil {
		return err
	}
	baseUrl.Path = "/"
	log.Printf("Updating %q", *update)

	// Start with the root file system because writing to the non-active
	// partition cannot break the currently running system.
	if err := updater.UpdateRoot(baseUrl.String(), rootReader); err != nil {
		return fmt.Errorf("updating root file system: %v", err)
	}

	if err := updater.UpdateBoot(baseUrl.String(), bootReader); err != nil {
		return fmt.Errorf("updating boot file system: %v", err)
	}

	if err := updater.Switch(baseUrl.String()); err != nil {
		return fmt.Errorf("switching to non-active partition: %v", err)
	}

	if err := updater.Reboot(baseUrl.String()); err != nil {
		return fmt.Errorf("reboot: %v", err)
	}

	log.Printf("updated, should be back within 10 seconds")
	return nil
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage)
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	gokrazyPkgs = strings.Split(*gokrazyPkgList, ",")

	if *overwrite == "" && *overwriteBoot == "" && *overwriteRoot == "" && *overwriteInit == "" && *update == "" {
		flag.Usage()
	}

	if err := logic(); err != nil {
		log.Fatal(err)
	}
}
