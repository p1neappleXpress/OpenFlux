package phphost

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeFTP is a small in-memory FTP server, enough for the client library: plain
// FTP, passive mode, a file tree, and knobs to make things go wrong.
type fakeFTP struct {
	ln   net.Listener
	user string
	pass string

	mu       sync.Mutex
	files    map[string][]byte // "/htdocs/index.php" -> data
	dirs     map[string]bool   // "/htdocs"
	readOnly bool              // STOR answers 550
	truncate bool              // STOR keeps only half the bytes
	abort    map[string]int    // path -> STORs left to cut short with "451 Transfer aborted" (as InfinityFree's Pure-FTPd did)
	stors    map[string]int    // path -> STORs begun
}

func newFakeFTP(t *testing.T, user, pass string, dirs ...string) *fakeFTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFTP{ln: ln, user: user, pass: pass, files: map[string][]byte{}, dirs: map[string]bool{"/": true},
		abort: map[string]int{}, stors: map[string]int{}}
	for _, d := range dirs {
		f.dirs["/"+strings.Trim(d, "/")] = true
	}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeFTP) addr() (host string, port int) {
	a := f.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (f *fakeFTP) target() FTP {
	h, p := f.addr()
	return FTP{Host: h, Port: p, User: f.user, Password: f.pass, TLS: "none"}
}

func (f *fakeFTP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.session(c)
	}
}

func clean(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if !strings.HasPrefix(p, "/") {
		p = path.Join(cwd, p)
	}
	return path.Clean(p)
}

func (f *fakeFTP) session(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	say("220 fake ftp ready")
	cwd := "/"
	authed, userOK := false, false
	var pasv net.Listener
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			userOK = arg == f.user
			say("331 password please")
		case "PASS":
			if userOK && arg == f.pass {
				authed = true
				say("230 welcome")
			} else {
				say("530 Login incorrect.")
			}
		case "AUTH":
			say("500 no TLS here")
		case "FEAT":
			say("211-Features:\r\n SIZE\r\n UTF8\r\n211 End")
		case "OPTS", "NOOP", "TYPE":
			say("200 ok")
		case "SYST":
			say("215 UNIX Type: L8")
		case "PWD":
			say(fmt.Sprintf(`257 "%s"`, cwd))
		case "CWD":
			p := clean(cwd, arg)
			f.mu.Lock()
			ok := f.dirs[p]
			f.mu.Unlock()
			if ok {
				cwd = p
				say("250 ok")
			} else {
				say("550 no such directory")
			}
		case "QUIT":
			say("221 bye")
			return
		case "PASV", "EPSV":
			if !authed {
				say("530 log in first")
				continue
			}
			if pasv != nil {
				pasv.Close()
			}
			pasv, _ = net.Listen("tcp", "127.0.0.1:0")
			port := pasv.Addr().(*net.TCPAddr).Port
			if strings.ToUpper(cmd) == "EPSV" {
				say(fmt.Sprintf("229 Entering Extended Passive Mode (|||%d|)", port))
			} else {
				say(fmt.Sprintf("227 Entering Passive Mode (127,0,0,1,%d,%d)", port>>8, port&255))
			}
		case "LIST", "NLST":
			p := cwd
			if arg != "" && !strings.HasPrefix(arg, "-") {
				p = clean(cwd, arg)
			}
			f.mu.Lock()
			known := f.dirs[p]
			lines := f.listing(p, strings.ToUpper(cmd) == "NLST")
			f.mu.Unlock()
			if !known {
				say("550 no such directory")
				continue
			}
			say("150 here comes the list")
			conn, _ := pasv.Accept()
			io.WriteString(conn, lines)
			conn.Close()
			say("226 done")
		case "STOR":
			p := clean(cwd, arg)
			f.mu.Lock()
			ro := f.readOnly
			f.mu.Unlock()
			if ro {
				say("550 permission denied")
				continue
			}
			say("150 send it")
			conn, _ := pasv.Accept()
			f.mu.Lock()
			f.stors[p]++
			cut := f.abort[p] > 0
			if cut {
				f.abort[p]--
			}
			f.mu.Unlock()
			if cut { // take a little, keep it, and hang up on the rest
				part := make([]byte, 1024)
				n, _ := io.ReadFull(conn, part)
				conn.Close()
				f.mu.Lock()
				f.files[p] = part[:n]
				f.mu.Unlock()
				say(`451 Transfer aborted\n0.458 seconds (measured here), 1.40 Mbytes per second`)
				continue
			}
			b, _ := io.ReadAll(conn)
			conn.Close()
			f.mu.Lock()
			if f.truncate {
				b = b[:len(b)/2]
			}
			f.files[p] = b
			f.mu.Unlock()
			say("226 stored")
		case "RETR":
			p := clean(cwd, arg)
			f.mu.Lock()
			b, ok := f.files[p]
			f.mu.Unlock()
			if !ok {
				say("550 no such file")
				continue
			}
			say("150 sending")
			conn, _ := pasv.Accept()
			conn.Write(b)
			conn.Close()
			say("226 done")
		case "SIZE":
			f.mu.Lock()
			b, ok := f.files[clean(cwd, arg)]
			f.mu.Unlock()
			if ok {
				say(fmt.Sprintf("213 %d", len(b)))
			} else {
				say("550 no such file")
			}
		case "MKD":
			f.mu.Lock()
			f.dirs[clean(cwd, arg)] = true
			f.mu.Unlock()
			say("257 created")
		case "DELE":
			p := clean(cwd, arg)
			f.mu.Lock()
			_, ok := f.files[p]
			delete(f.files, p)
			f.mu.Unlock()
			if ok {
				say("250 deleted")
			} else {
				say("550 no such file")
			}
		case "RMD":
			f.mu.Lock()
			delete(f.dirs, clean(cwd, arg))
			f.mu.Unlock()
			say("250 removed")
		default:
			say("502 not implemented")
		}
	}
}

// listing renders the children of dir, as `ls -l` or plain names.
func (f *fakeFTP) listing(dir string, namesOnly bool) string {
	seen := map[string]string{} // name -> "d" | "-"
	size := map[string]int{}
	under := func(p string) (string, bool) {
		prefix := strings.TrimRight(dir, "/") + "/"
		if !strings.HasPrefix(p, prefix) || p == dir {
			return "", false
		}
		return strings.SplitN(strings.TrimPrefix(p, prefix), "/", 2)[0], true
	}
	for d := range f.dirs {
		if n, ok := under(d); ok {
			seen[n] = "d"
		}
	}
	for p, b := range f.files {
		if n, ok := under(p); ok {
			if _, isDir := seen[n]; !isDir {
				seen[n], size[n] = "-", len(b)
			}
		}
	}
	var names []string
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		if namesOnly {
			sb.WriteString(n + "\r\n")
			continue
		}
		perm := "-rw-r--r--"
		if seen[n] == "d" {
			perm = "drwxr-xr-x"
		}
		fmt.Fprintf(&sb, "%s 1 u g %d Jan 01 00:00 %s\r\n", perm, size[n], n)
	}
	return sb.String()
}
