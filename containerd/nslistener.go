package main

import (
	"encoding/gob"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/golang/glog"
	"github.com/hyperhq/runv/supervisor"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func nsListenerDaemon() {

	/* create own netns */
	if err := syscall.Unshare(syscall.CLONE_NEWNET); err != nil {
		glog.Error(err)
		return
	}

	childPipe := os.NewFile(uintptr(3), "child")
	enc := gob.NewEncoder(childPipe)
	dec := gob.NewDecoder(childPipe)

	// TODO(hukeping): need change
	netNsPath := "var/run/netns/1234565"
	go setupNetworkNsTrap(netNsPath)

	/* notify containerd to execute prestart hooks */
	if err := enc.Encode("init"); err != nil {
		glog.Error(err)
		return
	}

	/* after execute prestart hooks */
	var ready string
	if err := dec.Decode(&ready); err != nil {
		glog.Error(err)
		return
	}

	if ready != "init" {
		glog.Errorf("get incorrect init message: %s", ready)
		return
	}

	/* send interface info to containerd */
	infos := collectionInterfaceInfo()
	if err := enc.Encode(infos); err != nil {
		glog.Error(err)
		return
	}

	/* TODO: watch network setting */
	var exit string
	if err := dec.Decode(&exit); err != nil {
		glog.Error(err)
	}
}

func collectionInterfaceInfo() []supervisor.InterfaceInfo {
	infos := []supervisor.InterfaceInfo{}

	links, err := netlink.LinkList()
	if err != nil {
		glog.Error(err)
		return infos
	}

	for _, link := range links {
		if link.Type() != "veth" {
			// lo is here too
			continue
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			glog.Error(err)
			return infos
		}

		for _, addr := range addrs {
			info := supervisor.InterfaceInfo{
				Ip:        addr.IPNet.String(),
				Index:     link.Attrs().Index,
				PeerIndex: link.Attrs().ParentIndex,
			}
			glog.Infof("get interface %v", info)
			infos = append(infos, info)
		}
		// set link down, tap device take over it
		netlink.LinkSetDown(link)
	}
	return infos
}

type tearDownNetlink func()

func setUpNetlink(path string) tearDownNetlink {
	if os.Getuid() != 0 {
		msg := "Skipped test because it requires root privileges."
		logrus.Debug(msg)
		os.Exit(100)
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		logrus.Fatal("Failed to open file, error:", err)
	}

	// new temporary namespace so we don't pollute the host
	// lock thread since the namespace is thread local
	runtime.LockOSThread()

	// Save the current network namespace
	origns, _ := netns.Get()

	// Change the network namespace
	if err := netns.Set(netns.NsHandle(f.Fd())); err != nil {
		logrus.Fatal("Failed to create newns")
	}

	return func() {
		netns.Set(origns)
		runtime.UnlockOSThread()
	}
}

// This function should be put into the main process or somewhere that can be
// use to init the network namespace trap.
func setupNetworkNsTrap(path string) {

	tearDown := setUpNetlink(path)
	defer tearDown()

	// Subscribe for links change event
	chLink := make(chan netlink.LinkUpdate)
	doneLink := make(chan struct{})
	defer close(doneLink)
	if err := netlink.LinkSubscribe(chLink, doneLink); err != nil {
		logrus.Fatal(err)
	}

	// Subscribe for addresses change event
	chAddr := make(chan netlink.AddrUpdate)
	doneAddr := make(chan struct{})
	defer close(doneAddr)
	if err := netlink.AddrSubscribe(chAddr, doneAddr); err != nil {
		logrus.Fatal(err)
	}

	// Subscribe for route change event
	chRoute := make(chan netlink.RouteUpdate)
	doneRoute := make(chan struct{})
	defer close(doneRoute)
	if err := netlink.RouteSubscribe(chRoute, doneRoute); err != nil {
		logrus.Fatal(err)
	}

	for {
		select {
		case updateLink := <-chLink:
			handleLink(updateLink)
		case updateAddr := <-chAddr:
			handleAddr(updateAddr)
		case updateRoute := <-chRoute:
			handleRoute(updateRoute)
		}
	}
}

// Link specific, genarate a event to containerd or sth.
func handleLink(update netlink.LinkUpdate) {
	handle := func() {
		if update.IfInfomsg.Flags&syscall.IFF_UP == 1 {
			fmt.Printf("[Link device up]\t")
		} else {
			fmt.Printf("[Link device !up]\t")
		}

		// Maybe the event want some detail, it can be retrieved here.
		fmt.Printf("updateLink is:%+v, flag is:0x%x\n", update.Link.Attrs(), update.IfInfomsg.Flags)
	}
	HandleRTNetlinkChange(update.Link.Attrs().Index, handle)
}

// Address specific, genarate a event to containerd or sth.
func handleAddr(update netlink.AddrUpdate) {
	handle := func() {
		if update.NewAddr {
			fmt.Printf("[Add a address]")
		} else {
			fmt.Printf("[Delete a address]")
		}

		// Actually we need not to know whether it is a IPv4 or IPv6,
		// just in case we want to log it out.
		if update.LinkAddress.IP.To4() != nil {
			fmt.Printf("[IPv4]\t")
		} else {
			fmt.Printf("[IPv6]\t")
		}

		// Maybe the event want some detail, it can be retrieved here.
		fmt.Printf("updateAddr is:%+v\n", update)
	}
	HandleRTNetlinkChange(update.LinkIndex, handle)
}

// Route specific, genarate a event to containerd or sth.
func handleRoute(update netlink.RouteUpdate) {
	handle := func() {
		// Route type is not a bit mask for a couple of values, but a single
		// unsigned int, that's why we use switch here not the "&" operator.
		switch update.Type {
		case syscall.RTM_NEWROUTE:
			fmt.Printf("[Create a route]\t")
		case syscall.RTM_DELROUTE:
			fmt.Printf("[Remove a route]\t")
		case syscall.RTM_GETROUTE:
			fmt.Printf("[Receive info of a route]\t")
		}

		// Maybe the event want some detail, it can be retrieved here.
		fmt.Printf("updateRoute is:%+v\n", update)
	}
	HandleRTNetlinkChange(update.Route.LinkIndex, handle)
}

// HandleRTNetlinkChange handle the rtnetlink change event and can be used to
// do some verification.
//
// This function was designed as the entrance to deal with those change event,
// it will calling those specific handlers respectively.
// Personaly I thinks it is better to put all the dirty work into each handler,
// so that we can keep the entrance clear and simple.
func HandleRTNetlinkChange(linkIndex int, callback func()) {
	if err := sanityChecks(); err != nil {
		fmt.Println("Error happen when doing sanity check, error:", err)
		return
	}

	link, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		logrus.Error(err)
		return
	}
	logrus.Debugf("Events for link [%d][%s]", link.Attrs().Index, link.Attrs().Name)

	callback()
}

// Maybe some sanity check and some verifications
func sanityChecks() error {
	return nil
}
