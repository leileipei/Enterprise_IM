package policystore_test

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Linux control uses raw TCP relays to the same actual PostgreSQL, object
// gateway and TLS IdP. Only the download directory UID changes between cases.
// It executes the cross-compiled official API, never a test business handler.
func (f *fileBusinessProcessFixture) proveLinuxDownloadUID(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, e := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Architecture}}").Output()
	if e != nil {
		t.Fatal("mandatory Linux UID runtime unavailable")
	}
	arch := strings.TrimSpace(string(raw))
	switch arch {
	case "aarch64", "arm64":
		arch = "arm64"
	case "x86_64", "amd64":
		arch = "amd64"
	default:
		t.Fatal("unsupported mandatory Linux fixture architecture")
	}
	api := filepath.Join(f.privateRoot, "im-api-linux")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", api, "../../cmd/im-api")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, e := cmd.CombinedOutput(); e != nil {
		processPrivateFile(t, filepath.Join(f.privateRoot, "linux-build.log"), b)
		t.Fatal("official Linux API build failed")
	}
	source := filepath.Join(f.privateRoot, "relay.go")
	processPrivateFile(t, source, []byte(linuxProcessRelaySource))
	relay := filepath.Join(f.privateRoot, "relay-linux")
	cmd = exec.CommandContext(ctx, "go", "build", "-o", relay, source)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, e := cmd.CombinedOutput(); e != nil {
		processPrivateFile(t, filepath.Join(f.privateRoot, "relay-build.log"), b)
		t.Fatal("raw relay fixture build failed")
	}
	settings := f.apiEnvironment(false, true, "api-a")
	settings["IM_FILE_DOWNLOAD_OWNER_ID"] = freshFile().ID
	settings["SSL_CERT_FILE"] = "/fixture/oidc-ca.pem"
	settings["IM_FILE_DOWNLOAD_SPOOL_DIR"] = "/download"
	ports := []string{}
	for _, s := range []string{f.apiDSN, f.oidc.URL, f.objectGateway.URL} {
		u, e := url.Parse(s)
		if e != nil || u.Port() == "" {
			t.Fatal("Linux relay endpoint invalid")
		}
		ports = append(ports, u.Port())
	}
	settings["FIXTURE_TCP_PORTS"] = strings.Join(ports, ",")
	for _, uid := range []string{"0", "1234"} {
		settings["FIXTURE_EXPECT_UID"] = uid
		envFile := filepath.Join(f.privateRoot, "linux-"+uid+".env")
		var text strings.Builder
		for k, v := range settings {
			if strings.ContainsAny(v, "\r\n") {
				t.Fatal("invalid private fixture environment")
			}
			fmt.Fprintf(&text, "%s=%s\n", k, v)
		}
		processPrivateFile(t, envFile, []byte(text.String()))
		name := "enterprise-im-p426-uid-" + processRandom(t)[:12]
		args := []string{"run", "-d", "--name", name, "--label", "enterprise_im.stage=p4-26", "--label", "enterprise_im.case=" + f.schema, "--env-file", envFile, "--tmpfs", "/download:mode=0700,uid=" + uid, "-v", f.privateRoot + ":/fixture:ro", "alpine@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40", "/fixture/relay-linux"}
		if e := exec.Command("docker", args...).Run(); e != nil {
			t.Fatal("mandatory owned UID container failed to start")
		}
		t.Cleanup(func() {
			label, _ := exec.Command("docker", "inspect", "--format", `{{index .Config.Labels "enterprise_im.case"}}`, name).Output()
			if strings.TrimSpace(string(label)) == f.schema {
				_ = exec.Command("docker", "rm", "-f", name).Run()
			}
		})
		deadline := time.Now().Add(40 * time.Second)
		var log []byte
		matched := false
		for time.Now().Before(deadline) {
			log, _ = exec.Command("docker", "logs", name).CombinedOutput()
			if uid == "0" && productionAPIAddress(log) != "" {
				matched = true
				break
			}
			state, _ := exec.Command("docker", "inspect", "--format", "{{.State.Running}} {{.State.ExitCode}}", name).Output()
			if uid != "0" && strings.TrimSpace(string(state)) == "false 1" {
				matched = true
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		processPrivateFile(t, filepath.Join(f.privateRoot, "linux-uid-"+uid+".log"), log)
		if !matched {
			t.Fatal("Linux official UID control did not reach required result")
		}
		if !bytes.Contains(log, []byte("fixture_download_uid="+uid+" api_uid=0")) {
			t.Fatal("Linux fixture UID was not verified")
		}
		if uid == "0" {
			address := productionAPIAddress(log)
			response, e := exec.Command("docker", "exec", name, "wget", "-q", "-O", "-", "http://"+address+"/health/ready").Output()
			if e != nil || !bytes.Contains(response, []byte("ready")) {
				t.Fatal("positive Linux process dependencies not healthy")
			}
			if e = exec.Command("docker", "stop", "--time", "22", name).Run(); e != nil {
				t.Fatal("owned Linux control shutdown failed")
			}
		} else if productionAPIAddress(log) != "" {
			t.Fatal("foreign UID opened listener")
		}
	}
}

const linuxProcessRelaySource = `package main
import("fmt";"io";"net";"os";"os/exec";"os/signal";"strings";"syscall";"time")
func main(){
 st,e:=os.Stat("/download");if e!=nil{os.Exit(2)};uid:=st.Sys().(*syscall.Stat_t).Uid
 fmt.Printf("fixture_download_uid=%d api_uid=%d\n",uid,os.Geteuid())
 if fmt.Sprint(uid)!=os.Getenv("FIXTURE_EXPECT_UID")||os.Geteuid()!=0{os.Exit(2)}
 for _,port:=range strings.Split(os.Getenv("FIXTURE_TCP_PORTS"),","){
  listener,e:=net.Listen("tcp","127.0.0.1:"+port);if e!=nil{os.Exit(2)};defer listener.Close()
  go func(l net.Listener,p string){for{c,e:=l.Accept();if e!=nil{return};go func(){defer c.Close();up,e:=net.DialTimeout("tcp","host.docker.internal:"+p,time.Second);if e!=nil{return};defer up.Close();done:=make(chan struct{});go func(){io.Copy(up,c);up.Close();close(done)}();io.Copy(c,up);c.Close();<-done}()}}(listener,port)
 }
 child:=exec.Command("/fixture/im-api-linux");child.Env=os.Environ();child.Stdout=os.Stdout;child.Stderr=os.Stderr
 if child.Start()!=nil{os.Exit(2)};signals:=make(chan os.Signal,1);signal.Notify(signals,syscall.SIGINT,syscall.SIGTERM)
 go func(){s:=<-signals;child.Process.Signal(s)}()
 if e=child.Wait();e!=nil{if x,ok:=e.(*exec.ExitError);ok{os.Exit(x.ExitCode())};os.Exit(2)}
}`
