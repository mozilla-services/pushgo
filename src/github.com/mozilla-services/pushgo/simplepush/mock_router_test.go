// Automatically generated by MockGen. DO NOT EDIT!
// Source: src/github.com/mozilla-services/pushgo/simplepush/router.go

package simplepush

import (
	time "time"
	gomock "github.com/rafrombrc/gomock/gomock"
)

// Mock of Router interface
type MockRouter struct {
	ctrl     *gomock.Controller
	recorder *_MockRouterRecorder
}

// Recorder for MockRouter (not exported)
type _MockRouterRecorder struct {
	mock *MockRouter
}

func NewMockRouter(ctrl *gomock.Controller) *MockRouter {
	mock := &MockRouter{ctrl: ctrl}
	mock.recorder = &_MockRouterRecorder{mock}
	return mock
}

func (_m *MockRouter) EXPECT() *_MockRouterRecorder {
	return _m.recorder
}

func (_m *MockRouter) Start(_param0 chan<- error) {
	_m.ctrl.Call(_m, "Start", _param0)
}

func (_mr *_MockRouterRecorder) Start(arg0 interface{}) *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Start", arg0)
}

func (_m *MockRouter) Close() error {
	ret := _m.ctrl.Call(_m, "Close")
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockRouterRecorder) Close() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Close")
}

func (_m *MockRouter) Route(cancelSignal <-chan bool, uaid string, chid string, version int64, sentAt time.Time, logID string, data string) (bool, error) {
	ret := _m.ctrl.Call(_m, "Route", cancelSignal, uaid, chid, version, sentAt, logID, data)
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

func (_mr *_MockRouterRecorder) Route(arg0, arg1, arg2, arg3, arg4, arg5, arg6 interface{}) *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Route", arg0, arg1, arg2, arg3, arg4, arg5, arg6)
}

func (_m *MockRouter) Register(uaid string) error {
	ret := _m.ctrl.Call(_m, "Register", uaid)
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockRouterRecorder) Register(arg0 interface{}) *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Register", arg0)
}

func (_m *MockRouter) Unregister(uaid string) error {
	ret := _m.ctrl.Call(_m, "Unregister", uaid)
	ret0, _ := ret[0].(error)
	return ret0
}

func (_mr *_MockRouterRecorder) Unregister(arg0 interface{}) *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Unregister", arg0)
}

func (_m *MockRouter) URL() string {
	ret := _m.ctrl.Call(_m, "URL")
	ret0, _ := ret[0].(string)
	return ret0
}

func (_mr *_MockRouterRecorder) URL() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "URL")
}

func (_m *MockRouter) Status() (bool, error) {
	ret := _m.ctrl.Call(_m, "Status")
	ret0, _ := ret[0].(bool)
	ret1, _ := ret[1].(error)
	return ret0, ret1
}

func (_mr *_MockRouterRecorder) Status() *gomock.Call {
	return _mr.mock.ctrl.RecordCall(_mr.mock, "Status")
}
