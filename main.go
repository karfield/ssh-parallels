package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	termbox "github.com/nsf/termbox-go"
)

var helpText = `ssh-parallels [options]
--port, -p
	set ssh port, default is 22
--user, -u, -l
	set username to ssh login, default is root
--ask, -a
	ask username before login
--help, h
	display help content
`

func main() {
	port := flag.Int("port", 22, "")
	flag.IntVar(port, "p", 22, "")
	user := flag.String("user", "root", "")
	flag.StringVar(user, "u", "root", "")
	flag.StringVar(user, "l", "root", "")
	help := flag.Bool("help", false, "")
	flag.BoolVar(help, "h", false, "")
	ask := flag.Bool("ask", false, "")
	flag.BoolVar(ask, "a", false, "")
	flag.Parse()

	if *help {
		print(helpText)
		return
	}

	availableIps := []IpAddr{}
	for _, vm := range listVMs() {
		ips, err := vm.showIpAddress()
		if err != nil {
			continue
		}
		availableIps = append(availableIps, ips...)
	}

	if len(availableIps) == 0 {
		println("no available(running) vm found!")
		return
	}

	display := []string{}
	for _, ip := range availableIps {
		display = append(display, fmt.Sprintf(" %-20s: %-15s (%s)", ip.VM.Name, ip.IP.Ip, ip.Type))
	}

	lb := &listBox{
		title:   "Choose VM from parallels",
		content: display,
	}

	index, err := lb.displayAndSelect()
	if err != nil {
		if err == errCtrlC {
			println("cancel login to vm")
		}
		return
	}

	if index >= 0 && index < len(availableIps) {
		ip := availableIps[index]
		username := *user
		if *ask {
			var err error
			username, err = askForUsername(fmt.Sprintf("%s (%s)", ip.VM.Name, ip.IP.Ip))
			if err != nil {
				username = *user
			}
		}
		sshLogin(&ip, username, *port)
	}

}

type VM struct {
	ID         string `json:"ID"`
	Name       string `json:"name"`
	Type       string `json:"Type"`
	State      string `json:"State"`
	OS         string `json:"OS"`
	Uptime     string `json:"Uptime"`
	Home       string `json:"Home"`
	GuestTools struct {
		State   string `json:"state"`
		Version string `json:"version"`
	} `json:"GuestTools`
	Hardware map[string](map[string]interface{}) `json:"Hardware"`
}

func listVMs() []*VM {
	output, err := exec.Command("prlctl", "list", "-a", "--info", "-j").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Parallels is not installed!")
	}
	list := []*VM{}
	if err := json.Unmarshal(output, &list); err != nil {
		return nil
	}
	return list
}

var (
	uptimePattern     = regexp.MustCompile("(\\d+):(\\d+):(\\d+).*")
	guestToolsPattern = regexp.MustCompile("state=(\\w+) version=(.*)")
	nospacePattern    = regexp.MustCompile("([^ ]+)")
)

func (vm *VM) exec(args ...string) ([]byte, error) {
	args = append([]string{"exec", vm.ID}, args...)
	return exec.Command("prlctl", args...).Output()
}

type LinuxIpAddr struct {
	IfName    string
	LinkType  string
	Mac       string
	Ip        string
	Broadcast string
	Mask      string
}

type IpAddr struct {
	IP        *LinuxIpAddr
	VM        *VM
	Name      string
	IsDefault bool
	Type      string
}

var (
	beginLinePattern = regexp.MustCompile("(\\d+: |)(\\w+): .*")
	linkLinePattern  = regexp.MustCompile("link/(\\w+) ([\\w:]+) brd ([\\w:]+)")
	inetLinePattern  = regexp.MustCompile("inet ([\\d.]+)/(\\d+) brd ([\\d.]+) .*")
	// ether 00:1c:42:c4:5c:24  txqueuelen 1000  (Ethernet)
	etherLinkPattern = regexp.MustCompile("ether ([\\w:]+).*")
	// inet 10.211.55.5  netmask 255.255.255.0  broadcast 10.211.55.255
	inetLinePattern2 = regexp.MustCompile("inet ([\\d.]+)\\s+netmask\\s+([\\d.]+)\\s+broadcast\\s+([\\d.]+)")
)

func (vm *VM) testCommandExists(cmd string) bool {
	output, err := vm.exec("whereis", cmd)
	if err != nil {
		return false
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		s := scanner.Text()
		if strings.HasPrefix(s, cmd+":") {
			return strings.Trim(s[len(cmd+":"):], " ") != ""
		}
	}
	return false
}

func (vm *VM) getIps() ([]*LinuxIpAddr, error) {
	ips := []*LinuxIpAddr{}
	var output []byte
	err := fmt.Errorf("No command to get ip address")
	isIfconfig := false
	if vm.testCommandExists("ifconfig") {
		output, err = vm.exec("ifconfig")
		isIfconfig = true
	} else if vm.testCommandExists("ip") {
		output, err = vm.exec("ip", "address", "show")
	}
	if err != nil {
		return ips, err
	}

	var ip *LinuxIpAddr
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		s := scanner.Text()

		if len(s) == 0 {
			continue
		}

		if s[0] != ' ' && s[0] != '\t' {
			if ip != nil {
				ips = append(ips, ip)
			}
			ip = &LinuxIpAddr{}
			m := beginLinePattern.FindStringSubmatch(s)
			if len(m) > 0 {
				ip.IfName = m[2]
			}
			continue
		}

		if ip != nil {
			var m []string
			if isIfconfig {
				if m = inetLinePattern2.FindStringSubmatch(s); len(m) > 0 {
					ip.Ip = m[1]
					ip.Mask = m[2]
					ip.Broadcast = m[3]
				} else if m = etherLinkPattern.FindStringSubmatch(s); len(m) > 0 {
					ip.LinkType = "ether"
					ip.Mac = m[1]
				}
			} else {
				if m = inetLinePattern.FindStringSubmatch(s); len(m) > 0 {
					ip.Ip = m[1]
					ip.Mask = m[2]
					ip.Broadcast = m[3]
				} else if m = linkLinePattern.FindStringSubmatch(s); len(m) > 0 {
					ip.LinkType = m[1]
					ip.Mac = m[2]
				}
			}
		}
	}

	if ip != nil {
		ips = append(ips, ip)
	}

	return ips, nil
}

func compareMac(linuxMac, prlMac string) bool {
	m := strings.ToLower(prlMac)
	m = fmt.Sprintf("%s:%s:%s:%s:%s:%s", m[0:2], m[2:4], m[4:6], m[6:8], m[8:10], m[10:])
	return linuxMac == m
}

func (vm *VM) showIpAddress() ([]IpAddr, error) {
	addrs := []IpAddr{}
	if vm.State != "running" {
		return addrs, fmt.Errorf("%s(%s) not running", vm.Name, vm.ID)
	}

	var ips []*LinuxIpAddr
	var err error
	switch vm.OS {
	case "linux", "ubuntu":
		ips, err = vm.getIps()
		if err != nil {
			return addrs, err
		}
	default:
		return addrs, fmt.Errorf("%s is not supported", vm.OS)
	}

	for _, ip := range ips {
		for dev, params := range vm.Hardware {
			if strings.HasPrefix(dev, "net") {
				if params["enabled"] == true {
					mac, _ := params["mac"].(string)
					if compareMac(ip.Mac, mac) {
						typ, _ := params["type"].(string)
						addrs = append(addrs, IpAddr{
							VM:        vm,
							IP:        ip,
							Name:      dev,
							IsDefault: params["iface"] == "default",
							Type:      typ,
						})
						break
					}
				}
			}
		}
	}
	return addrs, nil
}

type listBox struct {
	title                string
	titlePosX, titlePosY int
	originX, originY     int
	centerX, centerY     int
	width                int
	height               int
	selectIndex          int
	content              []string
}

func (lb *listBox) drawContent() error {
	if lb.content == nil || len(lb.content) == 0 {
		return fmt.Errorf("miss content")
	}

	lb.height = len(lb.content) + 2
	for _, l := range lb.content {
		if len(l) > lb.width {
			lb.width = len(l) + 12
		}
	}
	if lb.width < len(lb.title) {
		lb.width = len(lb.title)
	}

	tw, th := termbox.Size()
	lb.centerX = tw / 2
	lb.centerY = th / 2

	lb.titlePosX = lb.centerX - len(lb.title)/2
	lb.titlePosY = lb.centerY - lb.height/2 - 2

	lb.originX = lb.centerX - lb.width/2
	lb.originY = lb.centerY - lb.height/2

	/* start to draw */
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)

	borderColor := termbox.ColorDefault
	titleColor := termbox.ColorGreen
	contentColor := termbox.ColorWhite
	cursorColor := termbox.ColorGreen

	// draw border
	termbox.SetCell(lb.originX, lb.originY, '┌', borderColor, borderColor)
	termbox.SetCell(lb.originX+lb.width-1, lb.originY, '┐', borderColor, borderColor)
	termbox.SetCell(lb.originX+lb.width-1, lb.originY+lb.height-1, '┘', borderColor, borderColor)
	termbox.SetCell(lb.originX, lb.originY+lb.height-1, '└', borderColor, borderColor)
	for x := 1; x < lb.width-2; x++ {
		termbox.SetCell(x+lb.originX, lb.originY, '─', borderColor, borderColor)
		termbox.SetCell(x+lb.originX, lb.originY+lb.height-1, '─', borderColor, borderColor)
	}
	for y := 1; y < lb.height-1; y++ {
		termbox.SetCell(lb.originX+lb.width-1, lb.originY+y, '|', borderColor, borderColor)
		termbox.SetCell(lb.originX, lb.originY+y, '|', borderColor, borderColor)
	}
	// draw title
	for i, t := range lb.title {
		termbox.SetCell(lb.titlePosX+i, lb.titlePosY, t, titleColor, termbox.ColorDefault)
	}

	for y, l := range lb.content {
		if y == lb.selectIndex {
			termbox.SetCell(lb.originX+2, lb.originY+1+y, '>', cursorColor, termbox.ColorDefault)
		}
		for x, r := range l {
			termbox.SetCell(lb.originX+4+x, lb.originY+1+y, r, contentColor, termbox.ColorDefault)
		}
	}

	termbox.Flush()

	return nil
}

func (lb *listBox) moveDown() {
	if lb.selectIndex < len(lb.content)-1 {
		lb.selectIndex++
		lb.drawContent()
	}
}

func (lb *listBox) moveUp() {
	if lb.selectIndex > 0 {
		lb.selectIndex--
		lb.drawContent()
	}
}

var errCtrlC = errors.New("ctrlC")

func (lb *listBox) displayAndSelect() (int, error) {
	err := termbox.Init()
	if err != nil {
		return -1, err
	}
	defer termbox.Close()

	termbox.SetInputMode(termbox.InputEsc)

	lb.drawContent()

	for {
		switch ev := termbox.PollEvent(); ev.Type {
		case termbox.EventResize:
			lb.drawContent()
		case termbox.EventKey:
			moved := false
			switch ev.Key {
			case termbox.KeyCtrlC:
				return lb.selectIndex, errCtrlC
			case termbox.KeyArrowUp, termbox.KeyArrowLeft, termbox.KeyPgup:
				lb.moveUp()
				moved = true
			case termbox.KeyArrowDown, termbox.KeyArrowRight, termbox.KeyPgdn:
				lb.moveDown()
				moved = true
			case termbox.KeyEsc:
				return 0, errCtrlC
			case termbox.KeyEnter:
				return lb.selectIndex, nil
			}
			if !moved {
				switch ev.Ch {
				case 'q', 'Q':
					return 0, errCtrlC
				case 'k', 'K':
					lb.moveUp()
				case 'j', 'J':
					lb.moveDown()
				}
			}
		case termbox.EventMouse:
			switch ev.Key {
			case termbox.MouseWheelUp:
				lb.moveUp()
			case termbox.MouseWheelDown:
				lb.moveDown()
			}
		case termbox.EventError:
			return -1, ev.Err
		}
	}
}

func askForUsername(info string) (string, error) {
	if err := termbox.Init(); err != nil {
		return "", err
	}
	defer termbox.Close()

	ww, wh := termbox.Size()
	cx, cy := ww/2, wh/2

	yourname := "Enter your username for " + info
	name := ""
	draw := func() {
		cd := termbox.ColorDefault
		width := len(name) + 2
		if width < 20 {
			width = 20
		}
		termbox.Clear(cd, cd)
		termbox.SetCell(cx-width/2, cy-1, '┌', cd, cd)
		termbox.SetCell(cx-width/2, cy+1, '└', cd, cd)
		termbox.SetCell(cx+width/2, cy-1, '┐', cd, cd)
		termbox.SetCell(cx+width/2, cy+1, '┘', cd, cd)
		termbox.SetCell(cx-width/2, cy, '|', cd, cd)
		termbox.SetCell(cx+width/2, cy, '|', cd, cd)
		for i := 0; i < width-1; i++ {
			termbox.SetCell(cx-width/2+1+i, cy-1, '-', cd, cd)
			termbox.SetCell(cx-width/2+1+i, cy+1, '-', cd, cd)
		}
		l := len(yourname)
		for i, r := range yourname {
			termbox.SetCell(cx-l/2+i, cy-2, r, termbox.ColorGreen, cd)
		}
		l = len(name)
		for i, r := range name {
			termbox.SetCell(cx-l/2+i, cy, r, cd, cd)
		}
		termbox.Flush()
	}

	draw()

entering:
	for {
		switch ev := termbox.PollEvent(); ev.Type {
		case termbox.EventResize:
			draw()
		case termbox.EventKey:
			switch ev.Key {
			case termbox.KeyCtrlC:
				return "", errCtrlC
			case termbox.KeyEsc:
				return "", errors.New("cancelled")
			case termbox.KeyBackspace, termbox.KeyBackspace2, termbox.KeyDelete:
				name = name[:len(name)-2]
				draw()
				continue entering
			case termbox.KeyEnter:
				return name, nil
			}
			name += string(ev.Ch)
			draw()
		}
	}

}

func sshLogin(ip *IpAddr, user string, port int) error {
	cmd := exec.Command("ssh", ip.IP.Ip, "-l", user, "-p", strconv.Itoa(port))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
