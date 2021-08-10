package main

import (
	"encoding/json"

	libDatabox "github.com/me-box/lib-go-databox"
)

type CMStore struct {
	Store *libDatabox.CoreStoreClient
}

const slaStoreID = "slaStore"

func NewCMStore(store *libDatabox.CoreStoreClient) *CMStore {

	//setup SLAStore
	store.RegisterDatasource(libDatabox.DataSourceMetadata{
		Description:    "Persistent SLA storage",
		ContentType:    "json",
		Vendor:         "databox",
		DataSourceType: "databox:container-manager:SLA",
		DataSourceID:   slaStoreID,
		StoreType:      "kv",
		IsActuator:     false,
		Location:       "",
		Unit:           "",
	})

	return &CMStore{Store: store}
}

func (s CMStore) SaveSLA(sla libDatabox.SLA) error {

	payload, err := json.Marshal(sla)
	if err != nil {
		return err
	}

	return s.Store.KVJSON.Write(slaStoreID, sla.Name, payload)

}

func (s CMStore) GetAllSLAs() ([]libDatabox.SLA, error) {

	var slaList []libDatabox.SLA

	keys, err := s.Store.KVJSON.ListKeys(slaStoreID)
	if err != nil {
		return nil, err
	}

	for _, k := range keys {
		var sla libDatabox.SLA
		payload, err := s.Store.KVJSON.Read(slaStoreID, k)
		if err != nil {
			libDatabox.Err("[GetAllSLAs] failed to get  " + slaStoreID + ". " + err.Error())
			continue
		}
		err = json.Unmarshal(payload, &sla)
		if err != nil {
			libDatabox.Err("[GetAllSLAs] failed decode SLA for " + slaStoreID + ". " + err.Error())
			continue
		}
		slaList = append(slaList, sla)
	}

	return slaList, err

}

func (s CMStore) DeleteSLA(name string) error {
	return s.Store.KVJSON.Delete(slaStoreID, name)
}

func (s CMStore) ClearSLADatabase() error {
	return s.Store.KVJSON.DeleteAll(slaStoreID)
}

func (s CMStore) SavePassword(password string) error {
	return s.Store.KVText.Write(slaStoreID, "CMPassword", []byte(password))
}

func (s CMStore) LoadPassword() (string, error) {
	password, err := s.Store.KVText.Read(slaStoreID, "CMPassword")
	return string(password), err
}
