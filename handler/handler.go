package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/firmware"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/localauth"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/reservation"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	"github.com/sirupsen/logrus"
)

// ConnectorInfo contains some simple state about a single connector.
type ConnectorInfo struct {
	Status             core.ChargePointStatus
	Availability       core.AvailabilityType
	CurrentTransaction int
	CurrentReservation int
}

func NewChargePointHandler(
	log *logrus.Logger,
	chargePoint ocpp16.ChargePoint,
	status core.ChargePointStatus,
	connectors map[int]*ConnectorInfo,
	errorCode core.ChargePointErrorCode,
	configuration ConfigMap,
	meterValue int,
	activePowerImport float32,
	localAuthList []localauth.AuthorizationData,
	localAuthListVersion int,
) *ChargePointHandler {
	return &ChargePointHandler{
		log:                  log,
		chargePoint:          chargePoint,
		status:               core.ChargePointStatusPreparing,
		connectors:           connectors,
		configuration:        configuration,
		meterValue:           meterValue,
		activePowerImport:    activePowerImport,
		errorCode:            errorCode,
		localAuthList:        localAuthList,
		localAuthListVersion: localAuthListVersion,
	}
}

// ChargePointHandler contains some simple state that a charge point needs to keep.
// In production this will typically be replaced by database/API calls.
type ChargePointHandler struct {
	log                  *logrus.Logger
	chargePoint          ocpp16.ChargePoint
	status               core.ChargePointStatus
	connectors           map[int]*ConnectorInfo
	errorCode            core.ChargePointErrorCode
	configuration        ConfigMap
	meterValue           int
	activePowerImport    float32
	localAuthList        []localauth.AuthorizationData
	localAuthListVersion int
	lastContextCancel    context.CancelFunc
}

var chargePoint ocpp16.ChargePoint

func (handler *ChargePointHandler) isValidConnectorID(ID int) bool {
	_, ok := handler.connectors[ID]
	return ok || ID == 0
}

func (handler *ChargePointHandler) StartTickerToSendStatusNotifications(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 10)
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				go func(chargePoint ocpp16.ChargePoint) {
					_, err := chargePoint.StatusNotification(1, core.NoError, handler.status)
					if err != nil {
						handler.log.Error(err)
					}
				}(handler.chargePoint)
			}
		}
	}(ctx)
}

func (handler *ChargePointHandler) StartTickerToSendMeterValues(ctx context.Context) {
	if val, ok := handler.configuration["MeterValueSampleInterval"]; ok {
		interval, err := strconv.Atoi(*val.Value)
		if err != nil {
			handler.log.Error(err)
			return
		}
		ticker := time.NewTicker(time.Second * time.Duration(interval))
		go func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					ticker.Stop()
					return
				case <-ticker.C:
					go func(chargePoint ocpp16.ChargePoint) {
						if handler.status == core.ChargePointStatusCharging {
							current, err := strconv.Atoi(*handler.configuration["ChargeRate"].Value)
							if err != nil {
								handler.log.Error(err)
							}
							handler.activePowerImport = 0.6875 * float32(current)
						} else {
							handler.activePowerImport = 0
						}
						connectorID := 1
						sampledValue := types.SampledValue{Value: fmt.Sprintf("%v", handler.activePowerImport), Unit: types.UnitOfMeasureWh, Format: types.ValueFormatRaw, Measurand: types.MeasurandPowerActiveImport, Context: types.ReadingContextSamplePeriodic, Location: types.LocationOutlet}
						meterval := types.MeterValue{
							Timestamp:    types.NewDateTime(time.Now()),
							SampledValue: []types.SampledValue{sampledValue},
						}
						conf, e := chargePoint.MeterValues(connectorID, []types.MeterValue{meterval})
						if e != nil {
							handler.log.Errorf("error triggering meter val, %v", e)
						}
						if conf == nil {
							handler.log.Error("conf is nil")
							return
						}
						handler.logDefault(core.MeterValuesFeatureName).Infof("%s sent", conf.GetFeatureName())
					}(handler.chargePoint)
				}
			}
		}(ctx)
	}
}

func (handler *ChargePointHandler) RFIDStartAndStopCharging(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 15)
	var errorCount int = 0
	const idtag string = "test_rfid"
	go func(ctx context.Context) {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				go func(chargePoint ocpp16.ChargePoint) {
					conf, err := chargePoint.Authorize(idtag)
					if err != nil {
						errorCount += 1
						handler.log.Error(err)
					}
					if conf == nil {
						handler.log.Error("conf is nil")
						return
					}
					handler.log.Infof("RFID authorize response %s", conf.IdTagInfo.Status)
					if conf.IdTagInfo.Status == types.AuthorizationStatusAccepted {
						if handler.connectors[1].CurrentTransaction == 0 {
							conf, err := chargePoint.StartTransaction(1, idtag, handler.meterValue, types.NewDateTime(time.Now()))
							if err != nil {
								errorCount += 1
								handler.log.Error(err)
							}

							handler.connectors[1].CurrentTransaction = conf.TransactionId
							handler.connectors[1].Status = core.ChargePointStatusCharging
							handler.status = core.ChargePointStatusCharging
						} else {
							_, err := chargePoint.StopTransaction(handler.meterValue, types.NewDateTime(time.Now()), handler.connectors[1].CurrentTransaction)
							if err != nil {
								errorCount += 1
								handler.log.Error(err)
							}
							handler.connectors[1].CurrentTransaction = 0
							handler.connectors[1].Status = core.ChargePointStatusPreparing
							handler.status = core.ChargePointStatusPreparing
						}

						_, err = handler.chargePoint.StatusNotification(1, core.NoError, handler.status)
						if err != nil {
							errorCount += 1
							handler.log.Info(err)
						}
					}
				}(handler.chargePoint)
			}
		}
	}(ctx)
}

// ------------- Core profile callbacks -------------

func (handler *ChargePointHandler) OnChangeAvailability(request *core.ChangeAvailabilityRequest) (confirmation *core.ChangeAvailabilityConfirmation, err error) {
	request.ConnectorId = 1
	if _, ok := handler.connectors[request.ConnectorId]; !ok {
		handler.logDefault(request.GetFeatureName()).Errorf("cannot change availability for invalid connector %v", request.ConnectorId)
		return core.NewChangeAvailabilityConfirmation(core.AvailabilityStatusRejected), nil
	}
	handler.connectors[request.ConnectorId].Availability = request.Type
	if request.Type == core.AvailabilityTypeInoperative {
		// TODO: stop ongoing transactions
		handler.connectors[request.ConnectorId].Status = core.ChargePointStatusUnavailable
	} else {
		handler.connectors[request.ConnectorId].Status = core.ChargePointStatusAvailable
	}
	handler.logDefault(request.GetFeatureName()).Infof("change availability for connector %v", request.ConnectorId)
	go handler.updateStatus(handler, request.ConnectorId, handler.connectors[request.ConnectorId].Status)
	return core.NewChangeAvailabilityConfirmation(core.AvailabilityStatusAccepted), nil
}

func (handler *ChargePointHandler) OnChangeConfiguration(request *core.ChangeConfigurationRequest) (confirmation *core.ChangeConfigurationConfirmation, err error) {
	configKey, ok := handler.configuration[request.Key]
	if !ok {
		handler.logDefault(request.GetFeatureName()).Errorf("couldn't change configuration for unsupported parameter %v", configKey.Key)
		return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusNotSupported), nil
	} else if configKey.Readonly {
		handler.logDefault(request.GetFeatureName()).Errorf("couldn't change configuration for readonly parameter %v", configKey.Key)
		return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusRejected), nil
	}
	configKey.Value = &request.Value
	handler.configuration[request.Key] = configKey
	handler.logDefault(request.GetFeatureName()).Infof("changed configuration for parameter %v to %v", configKey.Key, configKey.Value)
	if configKey.Key == MeterValueSampleInterval {
		if handler.lastContextCancel != nil {
			handler.lastContextCancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		handler.lastContextCancel = cancel
		handler.StartTickerToSendMeterValues(ctx)
	}
	return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusAccepted), nil
}

func (handler *ChargePointHandler) OnClearCache(request *core.ClearCacheRequest) (confirmation *core.ClearCacheConfirmation, err error) {
	handler.logDefault(request.GetFeatureName()).Infof("cleared mocked cache")
	return core.NewClearCacheConfirmation(core.ClearCacheStatusAccepted), nil
}

func (handler *ChargePointHandler) OnDataTransfer(request *core.DataTransferRequest) (confirmation *core.DataTransferConfirmation, err error) {
	handler.logDefault(request.GetFeatureName()).Infof("data transfer [Vendor: %v Message: %v]: %v", request.VendorId, request.MessageId, request.Data)
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (handler *ChargePointHandler) OnGetConfiguration(request *core.GetConfigurationRequest) (confirmation *core.GetConfigurationConfirmation, err error) {
	handler.log.Infof("recieved request: %+v", request)
	var resultKeys []core.ConfigurationKey
	var unknownKeys []string
	for _, key := range request.Key {
		configKey, ok := handler.configuration[key]
		if !ok {
			handler.log.Info("unknown key")
			unknownKeys = append(unknownKeys, *configKey.Value)
		} else {
			handler.log.Infof("config key: %v", configKey)
			resultKeys = append(resultKeys, configKey)
		}
	}
	if len(request.Key) == 0 {
		// Return config for all keys
		for _, v := range handler.configuration {
			resultKeys = append(resultKeys, v)
		}
	}

	handler.logDefault(request.GetFeatureName()).Infof("returning configuration for requested keys: %v", request.Key)
	conf := core.NewGetConfigurationConfirmation(resultKeys)
	if conf == nil {
		handler.log.Error("conf is nil")
		return
	}
	conf.UnknownKey = unknownKeys
	return conf, nil
}

func (handler *ChargePointHandler) OnRemoteStartTransaction(request *core.RemoteStartTransactionRequest) (confirmation *core.RemoteStartTransactionConfirmation, err error) {
	connectorID := 1
	connector, ok := handler.connectors[connectorID]
	if !ok {
		return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusRejected), nil
	} else if connector.Availability != core.AvailabilityTypeOperative || connector.Status != core.ChargePointStatusPreparing || connector.CurrentTransaction > 0 {
		return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusRejected), nil
	}
	handler.logDefault(request.GetFeatureName()).Infof("started transaction %v on connector %v", connector.CurrentTransaction, connectorID)
	go func() {
		conf, err := handler.chargePoint.StartTransaction(connectorID, request.IdTag, handler.meterValue, types.NewDateTime(time.Now()))
		if err != nil {
			handler.log.Error(err)
			return
		}
		if conf == nil {
			handler.log.Error("conf is nil")
			return
		}
		handler.log.Infof("%+v", conf)
		for _, val := range handler.connectors {
			val.CurrentTransaction = conf.TransactionId
			val.Status = core.ChargePointStatusCharging
			handler.status = core.ChargePointStatusCharging
		}
		_, err = handler.chargePoint.StatusNotification(1, core.NoError, handler.status)
		if err != nil {
			handler.log.Error(err)
		}
	}()
	return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil

	// handler.logDefault(request.GetFeatureName()).Errorf("couldn't start a transaction for %v without a connectorID", request.IdTag)
	// return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusRejected), nil
}

func (handler *ChargePointHandler) OnRemoteStopTransaction(request *core.RemoteStopTransactionRequest) (confirmation *core.RemoteStopTransactionConfirmation, err error) {
	for key, val := range handler.connectors {
		handler.log.Infof("current tx: %d, request tx: %d", val.CurrentTransaction, request.TransactionId)
		if val.CurrentTransaction == request.TransactionId {
			handler.logDefault(request.GetFeatureName()).Infof("stopped transaction %v on connector %v", val.CurrentTransaction, key)
			val.CurrentTransaction = 0
			val.CurrentReservation = 0
			handler.activePowerImport = 0
			val.Status = core.ChargePointStatusPreparing
			handler.status = core.ChargePointStatusPreparing
			go func() {
				conf, err := handler.chargePoint.StopTransaction(handler.meterValue, types.NewDateTime(time.Now()), request.TransactionId)
				if err != nil {
					handler.log.Error(err)
					return
				}
				handler.log.Infof("%+v", conf)
				_, err = handler.chargePoint.StatusNotification(1, core.NoError, handler.status)
				if err != nil {
					handler.log.Error(err)
				}
			}()
			return core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
		}
	}
	handler.logDefault(request.GetFeatureName()).Errorf("couldn't stop transaction %v, no such transaction is ongoing", request.TransactionId)
	return core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusRejected), nil
}

func (handler *ChargePointHandler) OnReset(request *core.ResetRequest) (confirmation *core.ResetConfirmation, err error) {
	//TODO: stop all ongoing transactions
	handler.logDefault(request.GetFeatureName()).Warn("no reset logic implemented yet")
	return core.NewResetConfirmation(core.ResetStatusAccepted), nil
}

func (handler *ChargePointHandler) OnUnlockConnector(request *core.UnlockConnectorRequest) (confirmation *core.UnlockConnectorConfirmation, err error) {
	_, ok := handler.connectors[request.ConnectorId]
	if !ok {
		handler.logDefault(request.GetFeatureName()).Errorf("couldn't unlock invalid connector %v", request.ConnectorId)
		return core.NewUnlockConnectorConfirmation(core.UnlockStatusNotSupported), nil
	}
	handler.logDefault(request.GetFeatureName()).Infof("unlocked connector %v", request.ConnectorId)
	return core.NewUnlockConnectorConfirmation(core.UnlockStatusUnlocked), nil
}

// ------------- Local authorization list profile callbacks -------------

func (handler *ChargePointHandler) OnGetLocalListVersion(request *localauth.GetLocalListVersionRequest) (confirmation *localauth.GetLocalListVersionConfirmation, err error) {
	handler.logDefault(request.GetFeatureName()).Infof("returning current local list version: %v", handler.localAuthListVersion)
	return localauth.NewGetLocalListVersionConfirmation(handler.localAuthListVersion), nil
}

func (handler *ChargePointHandler) OnSendLocalList(request *localauth.SendLocalListRequest) (confirmation *localauth.SendLocalListConfirmation, err error) {
	if request.ListVersion <= handler.localAuthListVersion {
		handler.logDefault(request.GetFeatureName()).Errorf("requested listVersion %v is lower/equal than the current list version %v", request.ListVersion, handler.localAuthListVersion)
		return localauth.NewSendLocalListConfirmation(localauth.UpdateStatusVersionMismatch), nil
	}
	if request.UpdateType == localauth.UpdateTypeFull {
		handler.localAuthList = request.LocalAuthorizationList
		handler.localAuthListVersion = request.ListVersion
	} else if request.UpdateType == localauth.UpdateTypeDifferential {
		handler.localAuthList = append(handler.localAuthList, request.LocalAuthorizationList...)
		handler.localAuthListVersion = request.ListVersion
	}
	handler.logDefault(request.GetFeatureName()).Errorf("accepted new local authorization list %v, %v", request.ListVersion, request.UpdateType)
	return localauth.NewSendLocalListConfirmation(localauth.UpdateStatusAccepted), nil
}

// ------------- Firmware management profile callbacks -------------

func (handler *ChargePointHandler) OnGetDiagnostics(request *firmware.GetDiagnosticsRequest) (confirmation *firmware.GetDiagnosticsConfirmation, err error) {
	//TODO: perform diagnostics upload out-of-band
	handler.logDefault(request.GetFeatureName()).Warn("no diagnostics upload logic implemented yet")
	return firmware.NewGetDiagnosticsConfirmation(), nil
}

func (handler *ChargePointHandler) OnUpdateFirmware(request *firmware.UpdateFirmwareRequest) (confirmation *firmware.UpdateFirmwareConfirmation, err error) {
	retries := 0
	retryInterval := 30
	if request.Retries != nil {
		retries = *request.Retries
	}
	if request.RetryInterval != nil {
		retryInterval = *request.RetryInterval
	}
	handler.logDefault(request.GetFeatureName()).Infof("starting update firmware procedure")
	go handler.updateFirmware(request.Location, request.RetrieveDate, retries, retryInterval)
	return firmware.NewUpdateFirmwareConfirmation(), nil
}

// ------------- Remote trigger profile callbacks -------------

func (handler *ChargePointHandler) OnTriggerMessage(request *remotetrigger.TriggerMessageRequest) (confirmation *remotetrigger.TriggerMessageConfirmation, err error) {
	handler.logDefault(request.GetFeatureName()).Infof("received trigger for %v", request.RequestedMessage)
	status := remotetrigger.TriggerMessageStatusRejected
	switch request.RequestedMessage {
	case core.BootNotificationFeatureName:
		//TODO: schedule boot notification message
		go func() {
			conf, e := chargePoint.BootNotification("TestModel", "Utsav")
			checkError(e)
			if conf == nil {
				handler.log.Error("conf is nil")
				return
			}
			handler.logDefault(core.BootNotificationFeatureName).Infof("boot notification sent: %v", conf.Status)
		}()
		status = remotetrigger.TriggerMessageStatusAccepted
	case firmware.DiagnosticsStatusNotificationFeatureName:
		// Schedule diagnostics status notification request
		go func() {
			_, e := chargePoint.DiagnosticsStatusNotification(firmware.DiagnosticsStatusIdle)
			checkError(e)
			handler.logDefault(firmware.DiagnosticsStatusNotificationFeatureName).Info("diagnostics status notified")
		}()
		status = remotetrigger.TriggerMessageStatusAccepted
	case firmware.FirmwareStatusNotificationFeatureName:
		//TODO: schedule firmware status notification message
		break
	case core.HeartbeatFeatureName:
		// Schedule heartbeat request
		go func() {
			conf, e := chargePoint.Heartbeat()
			checkError(e)
			if conf == nil {
				handler.log.Error("conf is nil")
				return
			}
			handler.logDefault(core.HeartbeatFeatureName).Infof("clock synchronized: %v", conf.CurrentTime.FormatTimestamp())
		}()
		status = remotetrigger.TriggerMessageStatusAccepted
	case core.MeterValuesFeatureName:
		// TODO: schedule meter values message
		go func(chargePoint ocpp16.ChargePoint) {
			connectorID := 1
			sampledValue := types.SampledValue{Value: fmt.Sprintf("%v", handler.activePowerImport), Unit: types.UnitOfMeasureWh, Format: types.ValueFormatRaw, Measurand: types.MeasurandPowerActiveImport, Context: types.ReadingContextSamplePeriodic, Location: types.LocationOutlet}
			meterval := types.MeterValue{
				Timestamp:    types.NewDateTime(time.Now()),
				SampledValue: []types.SampledValue{sampledValue},
			}
			conf, e := chargePoint.MeterValues(connectorID, []types.MeterValue{meterval})
			checkError(e)
			if conf == nil {
				handler.log.Error("conf is nil")
				return
			}
			handler.logDefault(core.MeterValuesFeatureName).Infof("%s sent", conf.GetFeatureName())
		}(handler.chargePoint)
		status = remotetrigger.TriggerMessageStatusAccepted
	case core.StatusNotificationFeatureName:
		connectorID := 1
		handler.log.Infof("%v", request)
		// Check if requested connector is valid and status can be retrieved
		if !handler.isValidConnectorID(connectorID) {
			handler.logDefault(request.GetFeatureName()).Errorf("cannot trigger %v: requested invalid connector %v", request.RequestedMessage, connectorID)
			return remotetrigger.NewTriggerMessageConfirmation(remotetrigger.TriggerMessageStatusRejected), nil
		}
		//  Schedule status notification request
		go func(chargePoint ocpp16.ChargePoint) {
			status := handler.status
			if c, ok := handler.connectors[connectorID]; ok {
				status = c.Status
			}
			handler.log.Infof("%v %v %v", connectorID, handler.errorCode, status)
			conf, e := chargePoint.StatusNotification(connectorID, handler.errorCode, status)
			checkError(e)
			if conf == nil {
				handler.log.Error("conf is nil")
				return
			}
			handler.logDefault(conf.GetFeatureName()).Infof("status for connector %v sent: %v", connectorID, status)
		}(handler.chargePoint)
		status = remotetrigger.TriggerMessageStatusAccepted
	default:
		return remotetrigger.NewTriggerMessageConfirmation(remotetrigger.TriggerMessageStatusNotImplemented), nil
	}
	return remotetrigger.NewTriggerMessageConfirmation(status), nil
}

// ------------- Reservation profile callbacks -------------

func (handler *ChargePointHandler) OnReserveNow(request *reservation.ReserveNowRequest) (confirmation *reservation.ReserveNowConfirmation, err error) {
	connector := handler.connectors[request.ConnectorId]
	if connector == nil {
		return reservation.NewReserveNowConfirmation(reservation.ReservationStatusUnavailable), nil
	} else if connector.Status != core.ChargePointStatusAvailable {
		return reservation.NewReserveNowConfirmation(reservation.ReservationStatusOccupied), nil
	}
	connector.CurrentReservation = request.ReservationId
	handler.logDefault(request.GetFeatureName()).Infof("reservation %v for connector %v accepted", request.ReservationId, request.ConnectorId)
	go handler.updateStatus(handler, request.ConnectorId, core.ChargePointStatusReserved)
	// TODO: automatically remove reservation after expiryDate
	return reservation.NewReserveNowConfirmation(reservation.ReservationStatusAccepted), nil
}

func (handler *ChargePointHandler) OnCancelReservation(request *reservation.CancelReservationRequest) (confirmation *reservation.CancelReservationConfirmation, err error) {
	for k, v := range handler.connectors {
		if v.CurrentReservation == request.ReservationId {
			v.CurrentReservation = 0
			if v.Status == core.ChargePointStatusReserved {
				go handler.updateStatus(handler, k, core.ChargePointStatusAvailable)
			}
			handler.logDefault(request.GetFeatureName()).Infof("reservation %v for connector %v canceled", request.ReservationId, k)
			return reservation.NewCancelReservationConfirmation(reservation.CancelReservationStatusAccepted), nil
		}
	}
	handler.logDefault(request.GetFeatureName()).Infof("couldn't cancel reservation %v: reservation not found!", request.ReservationId)
	return reservation.NewCancelReservationConfirmation(reservation.CancelReservationStatusRejected), nil
}

// ------------- Smart charging profile callbacks -------------

func (handler *ChargePointHandler) OnSetChargingProfile(request *smartcharging.SetChargingProfileRequest) (confirmation *smartcharging.SetChargingProfileConfirmation, err error) {
	//TODO: handle logic
	handler.logDefault(request.GetFeatureName()).Warn("no set charging profile logic implemented yet")
	return smartcharging.NewSetChargingProfileConfirmation(smartcharging.ChargingProfileStatusRejected), nil
}

func (handler *ChargePointHandler) OnClearChargingProfile(request *smartcharging.ClearChargingProfileRequest) (confirmation *smartcharging.ClearChargingProfileConfirmation, err error) {
	//TODO: handle logic
	handler.logDefault(request.GetFeatureName()).Warn("no clear charging profile logic implemented yet")
	return smartcharging.NewClearChargingProfileConfirmation(smartcharging.ClearChargingProfileStatusUnknown), nil
}

func (handler *ChargePointHandler) OnGetCompositeSchedule(request *smartcharging.GetCompositeScheduleRequest) (confirmation *smartcharging.GetCompositeScheduleConfirmation, err error) {
	//TODO: handle logic
	handler.logDefault(request.GetFeatureName()).Warn("no get composite schedule logic implemented yet")
	return smartcharging.NewGetCompositeScheduleConfirmation(smartcharging.GetCompositeScheduleStatusRejected), nil
}

func checkError(err error) {
	if err != nil {
		logrus.Fatal(err)
	}
}

func getExpiryDate(info *types.IdTagInfo) string {
	if info.ExpiryDate != nil {
		return fmt.Sprintf("authorized until %v", info.ExpiryDate.String())
	}
	return ""
}

func (handler *ChargePointHandler) updateStatus(stateHandler *ChargePointHandler, connector int, status core.ChargePointStatus, props ...func(request *core.StatusNotificationRequest)) {
	if connector == 0 {
		stateHandler.status = status
	} else {
		stateHandler.connectors[connector].Status = status
	}
	handler.log.Infof("error code: %v, connector: %v, status: %v", stateHandler.errorCode, connector, status)
	statusConfirmation, err := chargePoint.StatusNotification(connector, stateHandler.errorCode, status)
	checkError(err)
	if connector == 0 {
		handler.logDefault(statusConfirmation.GetFeatureName()).Infof("status for all connectors updated to %v", status)
	} else {
		handler.logDefault(statusConfirmation.GetFeatureName()).Infof("status for connector %v updated to %v", connector, status)
	}
}

func (handler *ChargePointHandler) updateFirmwareStatus(status firmware.FirmwareStatus, props ...func(request *firmware.FirmwareStatusNotificationRequest)) {
	statusConfirmation, err := chargePoint.FirmwareStatusNotification(status, props...)
	checkError(err)
	handler.logDefault(statusConfirmation.GetFeatureName()).Infof("firmware status updated to %v", status)
}

func (handler *ChargePointHandler) updateFirmware(location string, retrieveDate *types.DateTime, retries int, retryInterval int) {
	handler.updateFirmwareStatus(firmware.FirmwareStatusDownloading)
	err := downloadFile("/tmp/out.bin", location)
	if err != nil {
		handler.logDefault(firmware.UpdateFirmwareFeatureName).Errorf("error while downloading file %v", err)
		handler.updateFirmwareStatus(firmware.FirmwareStatusDownloadFailed)
		return
	}
	handler.updateFirmwareStatus(firmware.FirmwareStatusDownloaded)
	// Simulate installation
	handler.updateFirmwareStatus(firmware.FirmwareStatusInstalling)
	time.Sleep(time.Second * 5)
	// Notify completion
	handler.updateFirmwareStatus(firmware.FirmwareStatusInstalled)
}

func downloadFile(filepath string, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// Utility functions
func (handler *ChargePointHandler) logDefault(feature string) *logrus.Entry {
	return handler.log.WithField("message", feature)
}
