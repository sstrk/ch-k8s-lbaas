package testing

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/mock"
	"github.com/cloudandheat/cah-loadbalancer/pkg/model"
)

// TODO: use mockery

type MockPortMapper struct {
	mock.Mock
}

func NewMockPortMapper() (*MockPortMapper) {
	return new(MockPortMapper)
}

func (m *MockPortMapper) MapService(svc *corev1.Service) error {
	a := m.Called(svc)
	return a.Error(0)
}

func (m *MockPortMapper) UnmapService(id model.ServiceIdentifier) error {
	a := m.Called(id)
	return a.Error(0)
}

func (m *MockPortMapper) GetServiceL3Port(id model.ServiceIdentifier) (string, error) {
	a := m.Called(id)
	return a.String(0), a.Error(1)
}

func (m *MockPortMapper) GetLBConfiguration() error {
	a := m.Called()
	return a.Error(0)
}

func (m *MockPortMapper) GetUsedL3Ports() ([]string, error) {
	a := m.Called()
	return a.Get(0).([]string), a.Error(1)
}

func (m *MockPortMapper) SetAvailableL3Ports(portIDs []string) ([]model.ServiceIdentifier, error) {
	a := m.Called(portIDs)
	return a.Get(0).([]model.ServiceIdentifier), a.Error(1)
}
