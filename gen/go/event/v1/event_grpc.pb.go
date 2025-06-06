// Code generated by protoc-gen-go-grpc. DO NOT EDIT.
// versions:
// - protoc-gen-go-grpc v1.3.0
// - protoc             (unknown)
// source: event/v1/event.proto

package eventv1

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

const (
	EventService_RecordEvent_FullMethodName         = "/event.v1.EventService/RecordEvent"
	EventService_GetAllSessionEvents_FullMethodName = "/event.v1.EventService/GetAllSessionEvents"
)

// EventServiceClient is the client API for EventService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type EventServiceClient interface {
	// RecordEvent 记录一个通用事件
	RecordEvent(ctx context.Context, in *RecordEventRequest, opts ...grpc.CallOption) (*RecordEventResponse, error)
	// GetAllSessionEvents 获取指定会话的所有事件
	GetAllSessionEvents(ctx context.Context, in *GetAllSessionEventsRequest, opts ...grpc.CallOption) (*GetAllSessionEventsResponse, error)
}

type eventServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewEventServiceClient(cc grpc.ClientConnInterface) EventServiceClient {
	return &eventServiceClient{cc}
}

func (c *eventServiceClient) RecordEvent(ctx context.Context, in *RecordEventRequest, opts ...grpc.CallOption) (*RecordEventResponse, error) {
	out := new(RecordEventResponse)
	err := c.cc.Invoke(ctx, EventService_RecordEvent_FullMethodName, in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *eventServiceClient) GetAllSessionEvents(ctx context.Context, in *GetAllSessionEventsRequest, opts ...grpc.CallOption) (*GetAllSessionEventsResponse, error) {
	out := new(GetAllSessionEventsResponse)
	err := c.cc.Invoke(ctx, EventService_GetAllSessionEvents_FullMethodName, in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EventServiceServer is the server API for EventService service.
// All implementations should embed UnimplementedEventServiceServer
// for forward compatibility
type EventServiceServer interface {
	// RecordEvent 记录一个通用事件
	RecordEvent(context.Context, *RecordEventRequest) (*RecordEventResponse, error)
	// GetAllSessionEvents 获取指定会话的所有事件
	GetAllSessionEvents(context.Context, *GetAllSessionEventsRequest) (*GetAllSessionEventsResponse, error)
}

// UnimplementedEventServiceServer should be embedded to have forward compatible implementations.
type UnimplementedEventServiceServer struct {
}

func (UnimplementedEventServiceServer) RecordEvent(context.Context, *RecordEventRequest) (*RecordEventResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method RecordEvent not implemented")
}
func (UnimplementedEventServiceServer) GetAllSessionEvents(context.Context, *GetAllSessionEventsRequest) (*GetAllSessionEventsResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method GetAllSessionEvents not implemented")
}

// UnsafeEventServiceServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to EventServiceServer will
// result in compilation errors.
type UnsafeEventServiceServer interface {
	mustEmbedUnimplementedEventServiceServer()
}

func RegisterEventServiceServer(s grpc.ServiceRegistrar, srv EventServiceServer) {
	s.RegisterService(&EventService_ServiceDesc, srv)
}

func _EventService_RecordEvent_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(RecordEventRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EventServiceServer).RecordEvent(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: EventService_RecordEvent_FullMethodName,
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EventServiceServer).RecordEvent(ctx, req.(*RecordEventRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func _EventService_GetAllSessionEvents_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(GetAllSessionEventsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(EventServiceServer).GetAllSessionEvents(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: EventService_GetAllSessionEvents_FullMethodName,
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(EventServiceServer).GetAllSessionEvents(ctx, req.(*GetAllSessionEventsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// EventService_ServiceDesc is the grpc.ServiceDesc for EventService service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var EventService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "event.v1.EventService",
	HandlerType: (*EventServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "RecordEvent",
			Handler:    _EventService_RecordEvent_Handler,
		},
		{
			MethodName: "GetAllSessionEvents",
			Handler:    _EventService_GetAllSessionEvents_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "event/v1/event.proto",
}
