package controller

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog"

	"github.com/cloudandheat/cah-loadbalancer/pkg/model"
	"github.com/cloudandheat/cah-loadbalancer/pkg/openstack"
)

var (
	ErrServiceNotMapped = errors.New("Service not mapped")
	ErrNoSuitablePort   = errors.New("No suitable port available")
)

const (
	AnnotationInboundPort = "cah-loadbalancer.k8s.cloudandheat.com/inbound-port"
)

type PortMapper interface {
	// Map the given service to a port
	//
	// If required, this will allocate a new port through the backend used for
	// the port mapper.
	//
	// Any errors occuring during port provisioning will be reported back by
	// this method. If this method reports an error, the service is not mapped.
	MapService(svc *corev1.Service) error

	// Remove all allocations of the service from the bookkeeping and release
	// L3 ports which are not used anymore
	//
	// Note that the release of ports can only be observed by polling
	// GetUsedL3Ports.
	UnmapService(id model.ServiceIdentifier) error

	// Return the ID of the port to which the service is mapped
	//
	// Returns ErrServiceNotMapped if the service is currently not mapped.
	GetServiceL3Port(id model.ServiceIdentifier) (string, error)

	GetLBConfiguration() error

	// Return the list of IDs of the L3 ports which currently have at least one
	// mapped service.
	GetUsedL3Ports() ([]string, error)

	// Set the list with available L3 port IDs.
	//
	// Any service which is currently mapped to a port which is not in the list
	// of IDs passed to this method will be unmapped. The identifiers of the
	// affected services will be returned in the return value.
	SetAvailableL3Ports(portIDs []string) ([]model.ServiceIdentifier, error)
}

type PortMapperImpl struct {
	l3manager openstack.L3PortManager
	services  map[string]model.ServiceModel
	l3ports   map[string]model.L3Port
}

func NewPortMapper(l3manager openstack.L3PortManager) PortMapper {
	return &PortMapperImpl{
		l3manager: l3manager,
		services:  make(map[string]model.ServiceModel),
		l3ports:   make(map[string]model.L3Port),
	}
}

func (c *PortMapperImpl) getServiceKey(svc *corev1.Service) string {
	return model.FromService(svc).ToKey()
}

func (c *PortMapperImpl) createNewL3Port() (string, error) {
	portID, err := c.l3manager.ProvisionPort()
	if err != nil {
		return "", err
	}
	c.emplaceL3Port(portID)
	return portID, nil
}

func (c *PortMapperImpl) emplaceL3Port(portID string) {
	c.l3ports[portID] = model.L3Port{
		Allocations: make(map[int32]string),
	}
}

// An L3 port is suitable for a set of L4 port allocations if and only if it can
// satisfy all of them.
func (c *PortMapperImpl) isPortSuitableFor(l3port model.L3Port, ports []model.L4Port) bool {
	for _, l4port := range ports {
		if !l3port.L4PortFree(l4port) {
			return false
		}
	}
	return true
}

// Check if any of the managed L3 ports is suitable for the given set of L4
// ports and return the first one which matches.
//
// If none matches, returns an ErrNoSuitablePort.
func (c *PortMapperImpl) findL3PortFor(ports []model.L4Port) (string, error) {
	for portID, l3port := range c.l3ports {
		if c.isPortSuitableFor(l3port, ports) {
			return portID, nil
		}
	}

	return "", ErrNoSuitablePort
}

func (c *PortMapperImpl) MapService(svc *corev1.Service) error {
	var err error
	key := c.getServiceKey(svc)

	svcModel := model.ServiceModel{
		L3PortID: "",
		Ports:    make([]model.L4Port, len(svc.Spec.Ports)),
	}
	for i, k8sPort := range svc.Spec.Ports {
		svcModel.Ports[i] = model.L4Port{Protocol: k8sPort.Protocol, Port: k8sPort.Port}
	}

	portID := ""
	// first, see if the service has a preferred port
	if svc.Annotations != nil {
		portID, _ = svc.Annotations[AnnotationInboundPort]
		// yes, there is a preferred port
		// TODO: retrieve port information from backend to ensure that it really
		// exists!
		l3port, exists := c.l3ports[portID]
		if exists {
			// the port is already known and thus may have allocations. we have
			// to check if any allocations conflict
			if !c.isPortSuitableFor(l3port, svcModel.Ports) {
				// and they do! so we have to relocate the service to a
				// different port
				// TODO: it would be good if that caused an event on the Service
				klog.Warningf(
					"relocating service %q to a new port due to conflict on old port %s",
					key,
					portID)
				portID = ""
			}
		} else {
			c.emplaceL3Port(portID)
		}
	}

	// if the service did not give us a specific port to use, we have to look
	// further
	if portID == "" {
		// second, try to find an existing port with non-conflicting allocations
		portID, err = c.findL3PortFor(svcModel.Ports)
		if err == ErrNoSuitablePort {
			// if no existing port can fit the bill, we move on to create a new
			// port
			portID, err = c.createNewL3Port()
			if err != nil {
				// if that fails too, we simply cannot map the service.
				return err
			}
		} else if err != nil {
			return err
		}
	}

	svcModel.L3PortID = portID
	c.services[key] = svcModel
	l3port := c.l3ports[portID]
	for _, port := range svcModel.Ports {
		l3port.Allocations[port.Port] = key
	}

	return nil
}

func (c *PortMapperImpl) GetServiceL3Port(id model.ServiceIdentifier) (string, error) {
	svcModel, ok := c.services[id.ToKey()]
	if !ok {
		return "", ErrServiceNotMapped
	}
	return svcModel.L3PortID, nil
}

func (c *PortMapperImpl) GetLBConfiguration() error {
	return fmt.Errorf("Not implemented")
}

func (c *PortMapperImpl) GetUsedL3Ports() ([]string, error) {
	result := []string{}
	for id, l3port := range c.l3ports {
		if len(l3port.Allocations) == 0 {
			delete(c.l3ports, id)
			continue
		}
		result = append(result, id)
	}
	return result, nil
}

func (c *PortMapperImpl) UnmapService(id model.ServiceIdentifier) error {
	key := id.ToKey()
	delete(c.services, key)
	for _, l3port := range c.l3ports {
		for portNumber, user := range l3port.Allocations {
			if user == key {
				delete(l3port.Allocations, portNumber)
			}
		}
	}
	return nil
}

func (c *PortMapperImpl) SetAvailableL3Ports(portIDs []string) ([]model.ServiceIdentifier, error) {
	vlog := klog.V(4)

	validPorts := make(map[string]bool)
	for _, validID := range portIDs {
		if vlog {
			vlog.Infof("port %q is considered available", validID)
		}
		validPorts[validID] = true
	}
	vlog.Infof("%d ports are considered available", len(validPorts))

	result := make([]model.ServiceIdentifier, 0)
	for portID, l3port := range c.l3ports {
		// check if port is in the set of available ports
		if _, ok := validPorts[portID]; ok {
			vlog.Infof("port %q is valid, skipping", portID)
			continue
		}

		vlog.Infof(
			"port %q is not valid, evicting services from %d allocations",
			portID, len(l3port.Allocations),
		)

		// it is not! we have to force-evict the affected services
		for _, serviceKey := range l3port.Allocations {
			vlog.Infof("evicting service %q", serviceKey)
			_, exists := c.services[serviceKey]
			// we check for existence here to avoid returning the same service
			// more than once if it has multiple allocations
			if exists {
				delete(c.services, serviceKey)
				id, err := model.FromKey(serviceKey)
				if err != nil {
					panic(fmt.Sprintf("internal error: key %q is not valid", serviceKey))
				}
				result = append(result, id)
			}
		}

		delete(c.l3ports, portID)
	}

	return result, nil
}
