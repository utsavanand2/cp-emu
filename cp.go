package cp

import (
	"context"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/localauth"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/reservation"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocppj"
	"github.com/lorenzodonini/ocpp-go/ws"
	"github.com/sirupsen/logrus"
	"github.com/utsavanand2/cpemu/handler"
)

func CreateAndRunChargePoint(chargingStationID string, centralSystemURL string) error {
	log := logrus.New()
	dispatcher := ocppj.NewDefaultClientDispatcher(ocppj.NewFIFOClientQueue(0))
	client := ws.NewClient()
	endpoint := ocppj.NewClient(chargingStationID, client, dispatcher, nil, core.Profile, localauth.Profile, firmware.Profile, reservation.Profile, remotetrigger.Profile, smartcharging.Profile)
	chargePoint := ocpp16.NewChargePoint(chargingStationID, endpoint, client)
	connectors := map[int]*handler.ConnectorInfo{
		1: {Status: core.ChargePointStatusPreparing, Availability: core.AvailabilityTypeOperative, CurrentTransaction: 0},
	}

	handler := handler.NewChargePointHandler(log, chargePoint, core.ChargePointStatusPreparing, connectors, core.NoError, handler.GetDefaultConfig(), 100, 0.0, []localauth.AuthorizationData{}, 0)

	chargePoint.SetCoreHandler(handler)
	chargePoint.SetFirmwareManagementHandler(handler)
	chargePoint.SetLocalAuthListHandler(handler)
	chargePoint.SetReservationHandler(handler)
	chargePoint.SetRemoteTriggerHandler(handler)
	chargePoint.SetSmartChargingHandler(handler)

	ocppj.SetLogger(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := chargePoint.Start(centralSystemURL)
	if err != nil {
		return err
	}
	log.Infof("connected to central system at %v", centralSystemURL)

	_, err = chargePoint.BootNotification("test", "cmev")
	if err != nil {
		return err
	}
	handler.StartTickerToSendStatusNotifications(ctx)
	handler.StartTickerToSendMeterValues(ctx)
	// handler.RFIDStartAndStopCharging(ctx)
	return nil
}
