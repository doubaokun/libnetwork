/*
Package libnetwork provides the basic functionality and extension points to
create network namespaces and allocate interfaces for containers to use.

	// Create a new controller instance
	controller := libnetwork.New()

	// Select and configure the network driver
	networkType := "bridge"
	option := options.Generic{}
	err := controller.ConfigureNetworkDriver(networkType, option)
	if err != nil {
		return
	}

	netOptions := options.Generic{}
	// Create a network for containers to join.
	network, err := controller.NewNetwork(networkType, "network1", netOptions)
	if err != nil {
		return
	}

	// For each new container: allocate IP and interfaces. The returned network
	// settings will be used for container infos (inspect and such), as well as
	// iptables rules for port publishing. This info is contained or accessible
	// from the returned endpoint.
	ep, err := network.CreateEndpoint("Endpoint1", nil)
	if err != nil {
		return
	}

	// A container can join the endpoint by providing the container ID to the join
	// api which returns the sandbox key which can be used to access the sandbox
	// created for the container during join.
	_, err = ep.Join("container1")
	if err != nil {
		return
	}
*/
package libnetwork

import (
	"sync"

	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/libnetwork/sandbox"
	"github.com/docker/libnetwork/types"
)

// NetworkController provides the interface for controller instance which manages
// networks.
type NetworkController interface {
	// ConfigureNetworkDriver applies the passed options to the driver instance for the specified network type
	ConfigureNetworkDriver(networkType string, options interface{}) error

	// Create a new network. The options parameter carries network specific options.
	// Labels support will be added in the near future.
	NewNetwork(networkType, name string, options interface{}) (Network, error)

	// Networks returns the list of Network(s) managed by this controller.
	Networks() []Network

	// WalkNetworks uses the provided function to walk the Network(s) managed by this controller.
	WalkNetworks(walker NetworkWalker)

	// NetworkByName returns the Network which has the passed name, if it exists otherwise nil is returned
	NetworkByName(name string) Network

	// NetworkByID returns the Network which has the passed id, if it exists otherwise nil is returned
	NetworkByID(id string) Network
}

// NetworkWalker is a client provided function which will be used to walk the Networks.
// When the function returns true, the walk will stop.
type NetworkWalker func(nw Network) bool

type sandboxData struct {
	sandbox sandbox.Sandbox
	refCnt  int
}

type networkTable map[types.UUID]*network
type endpointTable map[types.UUID]*endpoint
type sandboxTable map[string]sandboxData

type controller struct {
	networks  networkTable
	drivers   driverTable
	sandboxes sandboxTable
	sync.Mutex
}

// New creates a new instance of network controller.
func New() NetworkController {
	return &controller{networkTable{}, enumerateDrivers(), sandboxTable{}, sync.Mutex{}}
}

func (c *controller) ConfigureNetworkDriver(networkType string, options interface{}) error {
	d, ok := c.drivers[networkType]
	if !ok {
		return NetworkTypeError(networkType)
	}
	return d.Config(options)
}

// NewNetwork creates a new network of the specified network type. The options
// are network specific and modeled in a generic way.
func (c *controller) NewNetwork(networkType, name string, options interface{}) (Network, error) {
	// Check if a driver for the specified network type is available
	d, ok := c.drivers[networkType]
	if !ok {
		return nil, ErrInvalidNetworkDriver
	}

	// Check if a network already exists with the specified network name
	c.Lock()
	for _, n := range c.networks {
		if n.name == name {
			c.Unlock()
			return nil, NetworkNameError(name)
		}
	}
	c.Unlock()

	// Construct the network object
	network := &network{
		name:      name,
		id:        types.UUID(stringid.GenerateRandomID()),
		ctrlr:     c,
		driver:    d,
		endpoints: endpointTable{},
	}

	// Create the network
	if err := d.CreateNetwork(network.id, options); err != nil {
		return nil, err
	}

	// Store the network handler in controller
	c.Lock()
	c.networks[network.id] = network
	c.Unlock()

	return network, nil
}

func (c *controller) Networks() []Network {
	c.Lock()
	defer c.Unlock()

	list := make([]Network, 0, len(c.networks))
	for _, n := range c.networks {
		list = append(list, n)
	}

	return list
}

func (c *controller) WalkNetworks(walker NetworkWalker) {
	for _, n := range c.Networks() {
		if walker(n) {
			return
		}
	}
}

func (c *controller) NetworkByName(name string) Network {
	var n Network

	if name != "" {
		s := func(current Network) bool {
			if current.Name() == name {
				n = current
				return true
			}
			return false
		}

		c.WalkNetworks(s)
	}

	return n
}

func (c *controller) NetworkByID(id string) Network {
	c.Lock()
	defer c.Unlock()
	if n, ok := c.networks[types.UUID(id)]; ok {
		return n
	}
	return nil
}

func (c *controller) sandboxAdd(key string) (sandbox.Sandbox, error) {
	c.Lock()
	defer c.Unlock()

	sData, ok := c.sandboxes[key]
	if !ok {
		sb, err := sandbox.NewSandbox(key)
		if err != nil {
			return nil, err
		}

		sData = sandboxData{sandbox: sb, refCnt: 1}
		c.sandboxes[key] = sData
		return sData.sandbox, nil
	}

	sData.refCnt++
	return sData.sandbox, nil
}

func (c *controller) sandboxRm(key string) {
	c.Lock()
	defer c.Unlock()

	sData := c.sandboxes[key]
	sData.refCnt--

	if sData.refCnt == 0 {
		sData.sandbox.Destroy()
		delete(c.sandboxes, key)
	}
}

func (c *controller) sandboxGet(key string) sandbox.Sandbox {
	c.Lock()
	defer c.Unlock()

	sData, ok := c.sandboxes[key]
	if !ok {
		return nil
	}

	return sData.sandbox
}
