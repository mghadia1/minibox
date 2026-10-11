// Command probe runs inside a test container and tries to get out. It is
// built static by the tests (testutil.Probe) and prints what it found.
//
//	probe chroot-escape PATH   the classic chroot escape, then: is PATH there?
//	probe alloc MIB            allocate and touch MIB mebibytes, then exit
//	probe spin SECONDS         keep one CPU busy for SECONDS of wall time
//	probe spawn N              start up to N sleeping children; print how many started
//	probe serve PORT           answer HTTP on PORT with "hello from HOSTNAME"
//	probe get URL              print the body at URL (retrying for 3 s), or exit 1
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: probe chroot-escape PATH")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "chroot-escape":
		chrootEscape(os.Args[2])
	case "alloc":
		alloc(number(os.Args[2]))
	case "spin":
		spin(number(os.Args[2]))
	case "spawn":
		spawn(number(os.Args[2]))
	case "serve":
		serve(os.Args[2])
	case "get":
		get(os.Args[2])
	default:
		fmt.Fprintln(os.Stderr, "probe: unknown test", os.Args[1])
		os.Exit(2)
	}
}

// chrootEscape: chroot into a subdirectory while keeping the current
// directory outside it, walk up with "..", and chroot to where that ends.
// Inside a plain chroot this reaches the real root. Inside a container
// made with pivot_root and the old root detached, it reaches only the
// container's own root.
func chrootEscape(marker string) {
	must(os.MkdirAll("/escape-dir", 0o755))
	must(syscall.Chdir("/"))
	must(syscall.Chroot("/escape-dir"))
	for range 64 {
		must(syscall.Chdir(".."))
	}
	must(syscall.Chroot("."))
	if _, err := os.Stat(marker); err == nil {
		fmt.Println("escaped")
		return
	}
	fmt.Println("contained")
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func number(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(2)
	}
	return n
}

// alloc touches every page, so the memory is really used, not only
// reserved.
func alloc(mib int) {
	blocks := make([][]byte, 0, mib)
	for range mib {
		b := make([]byte, 1<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		blocks = append(blocks, b)
	}
	fmt.Println("allocated", len(blocks), "MiB")
}

func spin(seconds int) {
	end := time.Now().Add(time.Duration(seconds) * time.Second)
	n := 0
	for time.Now().Before(end) {
		n++
	}
	fmt.Println("spun", n > 0)
}

// spawn starts children until one fails (at the pids limit, fork fails
// with EAGAIN), and prints how many started. They are left sleeping; the
// container's end takes them.
func spawn(n int) {
	started := 0
	for range n {
		_, err := syscall.ForkExec("/bin/sleep", []string{"sleep", "30"}, &syscall.ProcAttr{Files: []uintptr{0, 1, 2}})
		if err != nil {
			fmt.Println("started", started, "then", err)
			return
		}
		started++
	}
	fmt.Println("started", started)
}

func serve(port string) {
	name, _ := os.Hostname()
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello from %s", name)
	})
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		os.Exit(1)
	}
}

func get(url string) {
	client := http.Client{Timeout: time.Second}
	var err error
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var resp *http.Response
		if resp, err = client.Get(url); err == nil {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if rerr == nil {
				fmt.Print(string(body))
				return
			}
			err = rerr
		}
	}
	fmt.Fprintln(os.Stderr, "probe:", err)
	os.Exit(1)
}
