package utils

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"sync"

	cuecsv "cuelang.org/go/pkg/encoding/csv"
	"github.com/gocarina/gocsv"
	"github.com/meshery/schemas/models/v1beta1/component"
	"google.golang.org/api/sheets/v4"
)

type SpreadsheetData struct {
	Model      *ModelCSV
	Components []component.ComponentDefinition
}

var (
	compBatchSize  = 100
	modelBatchSize = 100

	// refers to all cells in the fourth row (explain how is table clulte in ghsset and henc this rang is valid and will not reuire udpate or when it hould require update)
	ComponentsSheetAppendRange = "Components!A4"
	ModelsSheetAppendRange     = "Models!A4"
)

const XMLTAG = "<?xml version=\"1.0\" encoding=\"UTF-8\"?><!DOCTYPE svg>"

// registrant:model:component:[true/false]
// Tracks if component sheet requires update
var RegistrantToModelsToComponentsMap = make(map[string]map[string]map[string]bool)

// registrant:model:[true/false]
// Tracks if component sheet requires update
var RegistrantToModelsMap = make(map[string]map[string]bool)

func ProcessModelToComponentsMap(existingComponents map[string]map[string][]ComponentCSV) {
	RegistrantToModelsToComponentsMap = make(map[string]map[string]map[string]bool, len(existingComponents))
	for registrant, models := range existingComponents {
		for model, comps := range models {
			if RegistrantToModelsToComponentsMap[registrant] == nil {
				RegistrantToModelsToComponentsMap[registrant] = make(map[string]map[string]bool)
			}
			for _, comp := range comps {
				if RegistrantToModelsToComponentsMap[registrant][model] == nil {
					RegistrantToModelsToComponentsMap[registrant][model] = make(map[string]bool)
				}
				RegistrantToModelsToComponentsMap[registrant][model][comp.Component] = true
			}
		}
	}
}

func addEntriesInCompUpdateList(modelEntry *ModelCSV, compEntries []component.ComponentDefinition, compList []*ComponentCSV) []*ComponentCSV {
	registrant := modelEntry.Registrant
	model := modelEntry.Model

	if RegistrantToModelsToComponentsMap[registrant][model] == nil {
		RegistrantToModelsToComponentsMap[registrant][model] = make(map[string]bool)
	}

	for _, comp := range compEntries {
		if !RegistrantToModelsToComponentsMap[registrant][model][comp.Component.Kind] {
			RegistrantToModelsToComponentsMap[registrant][model][comp.Component.Kind] = true
			compList = append(compList, ConvertCompDefToCompCSV(modelEntry, comp))
			compBatchSize--
		}
	}

	return compList
}

func addEntriesInModelUpdateList(modelEntry *ModelCSV, modelList []*ModelCSV) []*ModelCSV {
	registrant := modelEntry.Registrant

	if RegistrantToModelsMap[registrant] == nil {
		RegistrantToModelsMap[registrant] = make(map[string]bool)
	}
	if registrant != "meshery" {
		RegistrantToModelsMap[registrant][modelEntry.Model] = true
		modelBatchSize--
	}

	return modelList
}

// Verifies if the component entry already exist in the spreadsheet, otherwise updates the spreadshhet to include new component entry.
func VerifyandUpdateSpreadsheet(cred string, wg *sync.WaitGroup, srv *sheets.Service, spreadsheetUpdateChan chan SpreadsheetData, sheetId string) {
	defer wg.Done()

	entriesToBeAddedInCompSheet := []*ComponentCSV{}
	entriesToBeAddedInModelSheet := []*ModelCSV{}

	for data := range spreadsheetUpdateChan {
		_, ok := RegistrantToModelsMap[data.Model.Registrant]
		if !ok {
			entriesToBeAddedInModelSheet = addEntriesInModelUpdateList(data.Model, entriesToBeAddedInModelSheet)
		}

		for _, comp := range data.Components {
			existingModels, ok := RegistrantToModelsToComponentsMap[data.Model.Registrant] // replace with registrantr
			if ok {
				existingComps, ok := existingModels[data.Model.Model]

				if ok {
					entryExist := existingComps[comp.Component.Kind]

					if !entryExist {
						entriesToBeAddedInCompSheet = append(entriesToBeAddedInCompSheet, ConvertCompDefToCompCSV(data.Model, comp))
						compBatchSize--
						RegistrantToModelsToComponentsMap[data.Model.Registrant][data.Model.Model][comp.Component.Kind] = true
					}
				} else {
					entriesToBeAddedInCompSheet = addEntriesInCompUpdateList(data.Model, data.Components, entriesToBeAddedInCompSheet)
				}
			} else {
				RegistrantToModelsToComponentsMap[data.Model.Registrant] = make(map[string]map[string]bool)
				entriesToBeAddedInCompSheet = addEntriesInCompUpdateList(data.Model, data.Components, entriesToBeAddedInCompSheet)
			}
		}

		if modelBatchSize <= 0 {
			// update model spreadsheet
			err := updateModelsSheet(srv, cred, sheetId, entriesToBeAddedInModelSheet)
			// Reset the list
			entriesToBeAddedInModelSheet = []*ModelCSV{}
			if err != nil {
				Log.Error(err)
			}
		}

		if compBatchSize <= 0 {
			// update comp spreadsheet
			err := updateComponentsSheet(srv, cred, sheetId, entriesToBeAddedInCompSheet)
			// Reset the list
			entriesToBeAddedInCompSheet = []*ComponentCSV{}
			entriesToBeAddedInModelSheet = []*ModelCSV{}
			if err != nil {
				Log.Error(err)
			}
		}
	}

	if len(entriesToBeAddedInModelSheet) > 0 {
		err := updateModelsSheet(srv, cred, sheetId, entriesToBeAddedInModelSheet)
		if err != nil {
			Log.Error(err)
		}
	}

	if len(entriesToBeAddedInCompSheet) > 0 {
		err := updateComponentsSheet(srv, cred, sheetId, entriesToBeAddedInCompSheet)
		if err != nil {
			Log.Error(err)
		}
		return
	}
}

func updateModelsSheet(srv *sheets.Service, cred, sheetId string, values []*ModelCSV) error {
	marshalledValues, err := marshalStructToCSValues[ModelCSV](values)
	if err != nil {
		return err
	}
	Log.Info("Appending", len(marshalledValues), "in the models sheet")
	err = appendSheet(srv, cred, sheetId, ModelsSheetAppendRange, marshalledValues)

	return err
}

func updateComponentsSheet(srv *sheets.Service, cred, sheetId string, values []*ComponentCSV) error {
	marshalledValues, err := marshalStructToCSValues[ComponentCSV](values)
	Log.Info("Appending", len(marshalledValues), "in the components sheet")
	if err != nil {
		return err
	}
	err = appendSheet(srv, cred, sheetId, ComponentsSheetAppendRange, marshalledValues)

	return err
}

func appendSheet(srv *sheets.Service, cred, sheetId, appendRange string, values [][]interface{}) error {

	if len(values) == 0 {
		return nil
	}
	_, err := srv.Spreadsheets.Values.Append(sheetId, appendRange, &sheets.ValueRange{
		MajorDimension: "ROWS",
		Range:          appendRange,
		Values:         values,
	}).InsertDataOption("INSERT_ROWS").ValueInputOption("RAW").Context(context.Background()).Do()

	if err != nil {
		return ErrAppendToSheet(err, sheetId)
	}
	return nil
}

func marshalStructToCSValues[K any](data []*K) ([][]interface{}, error) {
	csvString, err := gocsv.MarshalString(data)
	if err != nil {
		return nil, ErrMarshalStructToCSV(err)
	}
	csvReader := bytes.NewBufferString(csvString)
	decodedCSV, err := cuecsv.Decode(csvReader)

	if err != nil {
		return nil, ErrMarshalStructToCSV(err)
	}

	results := make([][]interface{}, 0)

	// The ouput is [ [col-names...] [row1][row2]]
	// delete the first entry i.e. [col-names..], as it contains the column names and is not required as we are concerened only with rows
	if len(decodedCSV) > 0 {
		for idx, val := range decodedCSV {
			if idx == 0 {
				continue
			}
			result := make([]interface{}, 0, cap(val))
			for _, r := range val {
				result = append(result, r)
			}
			results = append(results, result)
		}
		return results, nil
	}

	return results, nil
}

func DecodeAndProcessSVGString(escapedSVG string) (string, error) {
	decodedSVG, err := strconv.Unquote(`"` + escapedSVG + `"`)
	if err != nil {
		return "", err
	}

	r := strings.NewReader(decodedSVG)
	// Create a decoder for the SVG string.
	d := xml.NewDecoder(r)

	var b bytes.Buffer

	e := xml.NewEncoder(&b)

	for {
		t, err := d.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}

		if se, ok := t.(xml.StartElement); ok {
			for i, a := range se.Attr {
				if a.Name.Local == "someAttribute" {
					se.Attr[i].Value = "newValue"
				}
			}
			t = se
		}

		// Write the modified token to the buffer.
		if err := e.EncodeToken(t); err != nil {
			return "", err
		}
	}

	// Flush the encoder's buffer to the buffer.
	if err := e.Flush(); err != nil {
		return "", err
	}

	return XMLTAG + b.String(), nil
}
