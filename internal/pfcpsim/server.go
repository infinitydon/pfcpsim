// SPDX-License-Identifier: Apache-2.0
// Copyright 2022-present Open Networking Foundation

package pfcpsim

import (
        "context"
        "fmt"
        "net"

        "github.com/c-robinson/iplib"
        pb "github.com/infinitydon/pfcpsim/api"
        "github.com/infinitydon/pfcpsim/pkg/pfcpsim"
        "github.com/infinitydon/pfcpsim/pkg/pfcpsim/session"
        log "github.com/sirupsen/logrus"
        ieLib "github.com/wmnsk/go-pfcp/ie"
        "google.golang.org/grpc/codes"
        "google.golang.org/grpc/status"
)

// pfcpSimService implements the Protobuf interface and keeps a connection to a remote PFCP Agent peer.
// Its state is handled in internal/pfcpsim/state.go
type pfcpSimService struct{}

// SessionStep identifies the step in loops, used while creating/modifying/deleting sessions and rules IDs.
// It should be high enough to avoid IDs overlap when creating sessions. 5 Applications should be enough.
// In theory with ROC limitations, we should expect max 8 applications (5 explicit applications + 3 filters
// to deny traffic to the RFC1918 IPs, in case we have a ALLOW-PUBLIC)
const SessionStep = 10

func NewPFCPSimService(iface string) *pfcpSimService {
        interfaceName = iface
        return &pfcpSimService{}
}

func checkServerStatus() error {
        if !isConfigured() {
                return status.Error(codes.Aborted, "Server is not configured")
        }

        if !isRemotePeerConnected() {
                return status.Error(codes.Aborted, "Server is not associated")
        }

        return nil
}

func (P pfcpSimService) Configure(ctx context.Context, request *pb.ConfigureRequest) (*pb.Response, error) {
        if net.ParseIP(request.UpfN3Address) == nil {
                errMsg := fmt.Sprintf("Error while parsing UPF N3 address: %v", request.UpfN3Address)
                log.Error(errMsg)
                return &pb.Response{}, status.Error(codes.Aborted, errMsg)
        }
        // remotePeerAddress is validated in pfcpsim
        remotePeerAddress = request.RemotePeerAddress
        upfN3Address = request.UpfN3Address

        configurationMsg := fmt.Sprintf("Server is configured. Remote peer address: %v, N3 interface address: %v ", remotePeerAddress, upfN3Address)
        log.Info(configurationMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    configurationMsg,
        }, nil
}

func (P pfcpSimService) Associate(ctx context.Context, empty *pb.EmptyRequest) (*pb.Response, error) {
        if !isConfigured() {
                log.Error("Server is not configured")
                return &pb.Response{}, status.Error(codes.Aborted, "Server is not configured")
        }

        if !isRemotePeerConnected() {
                if err := connectPFCPSim(); err != nil {
                        errMsg := fmt.Sprintf("Could not connect to remote peer :%v", err)
                        log.Error(errMsg)
                        return &pb.Response{}, status.Error(codes.Aborted, errMsg)
                }
        }

        if err := sim.SetupAssociation(); err != nil {
                log.Error(err.Error())
                return &pb.Response{}, status.Error(codes.Aborted, err.Error())
        }

        infoMsg := "Association established"
        log.Info(infoMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    infoMsg,
        }, nil
}

func (P pfcpSimService) Disassociate(ctx context.Context, empty *pb.EmptyRequest) (*pb.Response, error) {
        if err := checkServerStatus(); err != nil {
                return &pb.Response{}, err
        }

        if err := sim.TeardownAssociation(); err != nil {
                log.Error(err.Error())
                return &pb.Response{}, status.Error(codes.Aborted, err.Error())
        }

        sim.DisconnectN4()

        remotePeerConnected = false

        infoMsg := "Association teardown completed and connection to remote peer closed"
        log.Info(infoMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    infoMsg,
        }, nil
}

func (P pfcpSimService) CreateSession(ctx context.Context, request *pb.CreateSessionRequest) (*pb.Response, error) {
        if err := checkServerStatus(); err != nil {
                return &pb.Response{}, err
        }

        baseID := int(request.BaseID)
        count := int(request.Count)

        lastUEAddr, _, err := net.ParseCIDR(request.UeAddressPool)
        if err != nil {
                errMsg := fmt.Sprintf(" Could not parse Address Pool: %v", err)
                log.Error(errMsg)
                return &pb.Response{}, status.Error(codes.Aborted, errMsg)
        }

        var qfi uint8 = 0

        if request.Qfi != 0 {
                qfi = uint8(request.Qfi)
        }

        if err = isNumOfAppFiltersCorrect(request.AppFilters); err != nil {
                return &pb.Response{}, err
        }

        for i := baseID; i < (count*SessionStep + baseID); i = i + SessionStep {
                // using variables to ease comprehension on how rules are linked together
                uplinkTEID := uint32(i)

                ueAddress := iplib.NextIP(lastUEAddr)
                lastUEAddr = ueAddress

                sessQerID := uint32(0)

                var pdrs, fars []*ieLib.IE

                qers := []*ieLib.IE{
                        // session QER
                        session.NewQERBuilder().
                                WithID(sessQerID).
                                WithMethod(session.Create).
                                WithUplinkMBR(60000).
                                WithDownlinkMBR(60000).
                                Build(),
                }

                // create as many PDRs, FARs and App QERs as the number of app filters provided through pfcpctl
                ID := uint16(i)

                for _, appFilter := range request.AppFilters {
                        SDFFilter, gateStatus, precedence, err := parseAppFilter(appFilter)
                        if err != nil {
                                return &pb.Response{}, status.Error(codes.Aborted, err.Error())
                        }

                        log.Infof("Successfully parsed application filter. SDF Filter: %v", SDFFilter)

                        uplinkPdrID := ID
                        downlinkPdrID := ID + 1

                        uplinkFarID := uint32(ID)
                        downlinkFarID := uint32(ID + 1)

                        uplinkAppQerID := uint32(ID)
                        downlinkAppQerID := uint32(ID + 1)

                        uplinkPDR := session.NewPDRBuilder().
                                WithID(uplinkPdrID).
                                WithMethod(session.Create).
                                WithTEID(uplinkTEID).
                                WithFARID(uplinkFarID).
                                AddQERID(sessQerID).
                                WithN3Address(upfN3Address).
                                WithSDFFilter(SDFFilter).
                                WithPrecedence(precedence).
                                MarkAsUplink().
                                BuildPDR()

                        downlinkPDR := session.NewPDRBuilder().
                                WithID(downlinkPdrID).
                                WithMethod(session.Create).
                                WithPrecedence(precedence).
                                WithUEAddress(ueAddress.String()).
                                WithSDFFilter(SDFFilter).
                                AddQERID(sessQerID).
                                WithFARID(downlinkFarID).
                                MarkAsDownlink().
                                BuildPDR()

                        pdrs = append(pdrs, uplinkPDR)
                        pdrs = append(pdrs, downlinkPDR)

                        uplinkFAR := session.NewFARBuilder().
                                WithID(uplinkFarID).
                                WithAction(session.ActionForward).
                                WithDstInterface(ieLib.DstInterfaceCore).
                                WithMethod(session.Create).
                                BuildFAR()

                        downlinkFAR := session.NewFARBuilder().
                                WithID(downlinkFarID).
                                WithAction(session.ActionForward).
                                WithMethod(session.Create).
                                WithDstInterface(ieLib.DstInterfaceAccess).
                                BuildFAR()

                        fars = append(fars, uplinkFAR)
                        fars = append(fars, downlinkFAR)

                        _ = session.NewQERBuilder().
                                WithID(uplinkAppQerID).
                                WithMethod(session.Create).
                                WithQFI(qfi).
                                WithUplinkMBR(50000).
                                WithDownlinkMBR(30000).
                                WithGateStatus(gateStatus).
                                Build()

                        _ = session.NewQERBuilder().
                                WithID(downlinkAppQerID).
                                WithMethod(session.Create).
                                WithQFI(qfi).
                                WithUplinkMBR(50000).
                                WithDownlinkMBR(30000).
                                WithGateStatus(gateStatus).
                                Build()

                        ID += 2
                }

                sess, err := sim.EstablishSession(pdrs, fars, qers)
                if err != nil {
                        return &pb.Response{}, status.Error(codes.Internal, err.Error())
                }
                insertSession(i, sess)
        }

        infoMsg := fmt.Sprintf("%v sessions were established using %v as baseID ", count, baseID)
        log.Info(infoMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    infoMsg,
        }, nil
}

func (P pfcpSimService) ModifySession(ctx context.Context, request *pb.ModifySessionRequest) (*pb.Response, error) {
        if err := checkServerStatus(); err != nil {
                return &pb.Response{}, err
        }

        // TODO add 5G mode
        baseID := int(request.BaseID)
        count := int(request.Count)
        nodeBaddress := request.NodeBAddress

        if len(activeSessions) < count {
                err := pfcpsim.NewNotEnoughSessionsError()
                log.Error(err)
                return &pb.Response{}, status.Error(codes.Aborted, err.Error())
        }

        var actions uint8 = 0

        if request.BufferFlag || request.NotifyCPFlag {
                // We currently support only both flags set
                actions |= session.ActionNotify
                actions |= session.ActionBuffer
        } else {
                // If no flag was passed, default action is Forward
                actions |= session.ActionForward
        }

        if err := isNumOfAppFiltersCorrect(request.AppFilters); err != nil {
                return &pb.Response{}, err
        }

        for i := baseID; i < (count*SessionStep + baseID); i = i + SessionStep {
                var newFARs []*ieLib.IE

                ID := uint32(i + 1)
                teid := uint32(i + 1)

                if request.BufferFlag || request.NotifyCPFlag {
                        teid = 0 // When buffering, TEID = 0.
                }

                for _, _ = range request.AppFilters {
                        downlinkFAR := session.NewFARBuilder().
                                WithID(ID). // Same FARID that was generated in create sessions
                                WithMethod(session.Update).
                                WithAction(actions).
                                WithDstInterface(ieLib.DstInterfaceAccess).
                                WithTEID(teid).
                                WithDownlinkIP(nodeBaddress).
                                BuildFAR()

                        newFARs = append(newFARs, downlinkFAR)

                        ID += 2
                }

                sess, ok := getSession(i)
                if !ok {
                        errMsg := fmt.Sprintf("Could not retrieve session with index %v", i)
                        log.Error(errMsg)
                        return &pb.Response{}, status.Error(codes.Internal, errMsg)
                }

                err := sim.ModifySession(sess, nil, newFARs, nil)
                if err != nil {
                        return &pb.Response{}, status.Error(codes.Internal, err.Error())
                }
        }

        infoMsg := fmt.Sprintf("%v sessions were modified", count)
        log.Info(infoMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    infoMsg,
        }, nil
}

func (P pfcpSimService) DeleteSession(ctx context.Context, request *pb.DeleteSessionRequest) (*pb.Response, error) {
        if err := checkServerStatus(); err != nil {
                return &pb.Response{}, err
        }

        baseID := int(request.BaseID)
        count := int(request.Count)

        if len(activeSessions) < count {
                err := pfcpsim.NewNotEnoughSessionsError()
                log.Error(err)
                return &pb.Response{}, status.Error(codes.Aborted, err.Error())
        }

        for i := baseID; i < (count*SessionStep + baseID); i = i + SessionStep {
                sess, ok := getSession(i)
                if !ok {
                        errMsg := "Session was nil. Check baseID"
                        log.Error(errMsg)
                        return &pb.Response{}, status.Error(codes.Aborted, errMsg)
                }

                err := sim.DeleteSession(sess)
                if err != nil {
                        log.Error(err.Error())
                        return &pb.Response{}, status.Error(codes.Aborted, err.Error())
                }
                // remove from activeSessions
                deleteSession(i)
        }

        infoMsg := fmt.Sprintf("%v sessions deleted; activeSessions: %v", count, len(activeSessions))
        log.Info(infoMsg)

        return &pb.Response{
                StatusCode: int32(codes.OK),
                Message:    infoMsg,
        }, nil
}
