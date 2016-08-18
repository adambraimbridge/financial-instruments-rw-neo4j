package financialinstruments

import (
	"github.com/Financial-Times/neo-utils-go/neoutils"
	"github.com/jmcvetta/neoism"
	"fmt"
	"encoding/json"
	"github.com/Financial-Times/up-rw-app-api-go/rwapi"
)

type service struct {
	cypherRunner neoutils.CypherRunner
	indexManager neoutils.IndexManager
}

const batchSize = 4096

//NewCypherFinancialInstrumentService returns a new service responsible for writing financial instruments in Neo4j
func NewCypherFinancialInstrumentService(cypherRunner neoutils.CypherRunner, indexManager neoutils.IndexManager) service {
	return service{
		cypherRunner: cypherRunner,
		indexManager: indexManager,
	}
}

func (s service) Initialise() error {
	err := neoutils.EnsureIndexes(s.indexManager, map[string]string{
		"Identifier": "value",
	})

	if err != nil {
		return err
	}

	return neoutils.EnsureConstraints(s.indexManager, map[string]string{
		"Thing":               "uuid",
		"Concept":             "uuid",
		"FinancialInstrument": "uuid",
		"Equity":              "uuid",
		"UPPIdentifier":       "value",
		"FactsetIdentifier":   "value",
		"FIGIIdentifier":      "value",
	})
}

func (s service) Read(uuid string) (interface{}, bool, error) {

	results := []financialInstrument{}

	readQuery := &neoism.CypherQuery{
		Statement: `MATCH (fi:FinancialInstrument {uuid:{uuid}})
				OPTIONAL MATCH (fi)-[:ISSUED_BY]->(org:Thing)
				OPTIONAL MATCH (upp:UPPIdentifier)-[:IDENTIFIES]->(fi)
				OPTIONAL MATCH (factset:FactsetIdentifier)-[:IDENTIFIES]->(fi)
				OPTIONAL MATCH (figi:FIGIIdentifier)-[:IDENTIFIES]->(fi)
				OPTIONAL MATCH (wsod:WSODIdentifier)-[:IDENTIFIES]->(fi)
				return fi.uuid as uuid,
					fi.prefLabel as prefLabel,
					org.uuid as issuedBy,
					{uuids:collect(distinct upp.value),
					figiCode:figi.value,
					factsetIdentifier:factset.value,
					wsodIdentifier: wsod.value} as alternativeIdentifiers`,
		Parameters:map[string]interface{}{
			"uuid": uuid,
		},
		Result: &results,
	}

	if err := s.cypherRunner.CypherBatch([]*neoism.CypherQuery{readQuery}); err != nil || len(results) == 0 {
		return financialInstrument{}, false, err
	}

	return results[0], true, nil

}

func createNewIdentifierQuery(uuid string, identifierLabel string, identifierValue string) *neoism.CypherQuery {
	statementTemplate := fmt.Sprintf(`MERGE (t:Thing {uuid:{uuid}})
				CREATE (i:Identifier {value:{value}})
				MERGE (t)<-[:IDENTIFIES]-(i)
				set i : %s`, identifierLabel)

	query := &neoism.CypherQuery{
		Statement: statementTemplate,
		Parameters: map[string]interface{}{
			"uuid": uuid,
			"value": identifierValue,
		},
	}
	return query
}

func (s service) Write(thing interface{}) error {

	hash, err := writeHash(thing)
	if err != nil {
		return err
	}

	fi := thing.(financialInstrument)

	params := map[string]interface{}{
		"uuid": fi.UUID,
		"hash": hash,
	}

	if fi.PrefLabel != "" {
		params["prefLabel"] = fi.PrefLabel
	}

	queries := []*neoism.CypherQuery{}

	deleteEntityRelationshipsQuery := &neoism.CypherQuery{
		Statement: `MATCH (t:Thing {uuid:{uuid}})
				OPTIONAL MATCH (t)-[is:ISSUED_BY]->(org:Thing)
				OPTIONAL MATCH (i:Identifier)-[ir:IDENTIFIES]->(t)
				DELETE ir, is, i`,
		Parameters: map[string]interface{}{
			"uuid": fi.UUID,
		},
	}
	queries = append(queries, deleteEntityRelationshipsQuery)

	writeQuery := &neoism.CypherQuery{
		Statement: `MERGE (t:Thing{uuid: {uuid}})
			set t={props}
			set t :Concept
			set t :FinancialInstrument
			set t :Equity`,
		Parameters: map[string]interface{}{
			"uuid": fi.UUID,
			"props": params,
		},
	}
	queries = append(queries, writeQuery)

	//ADD all the IDENTIFIER nodes and IDENTIFIES relationships
	for _, alternativeUUID := range fi.AlternativeIdentifiers.UUIDS {
		if alternativeUUID != "" {
			alternativeIdentifierQuery := createNewIdentifierQuery(fi.UUID, uppIdentifierLabel, alternativeUUID)
			queries = append(queries, alternativeIdentifierQuery)
		}
	}

	if fi.AlternativeIdentifiers.FactsetIdentifier != "" {
		queries = append(queries, createNewIdentifierQuery(fi.UUID, factsetIdentifierLabel, fi.AlternativeIdentifiers.FactsetIdentifier))
	}

	if fi.AlternativeIdentifiers.FIGICode != "" {
		queries = append(queries, createNewIdentifierQuery(fi.UUID, figiIdentifierLabel, fi.AlternativeIdentifiers.FIGICode))
	}

	if fi.AlternativeIdentifiers.WSODIdentifier != "" {
		queries = append(queries, createNewIdentifierQuery(fi.UUID, wsodIdentifierLabel, fi.AlternativeIdentifiers.WSODIdentifier))
	}

	if fi.IssuedBy != "" {
		orgUuid := fi.IssuedBy

		orgResults := []struct {
			UUID string `json:"uuid"`
		}{}

		findOrganisationQuery := &neoism.CypherQuery{
			Statement: `MATCH (i:Identifier {value: {uuid}})-[:IDENTIFIES]->(org:Thing) RETURN org.uuid as uuid`,
			Parameters: map[string]interface{}{
				"uuid": fi.IssuedBy,
			},
			Result: &orgResults,
		}

		if err := s.cypherRunner.CypherBatch([]*neoism.CypherQuery{findOrganisationQuery}); err != nil {
			fmt.Println(err)
			return err
		}

		if len(orgResults) > 0 {
			orgUuid = orgResults[0].UUID
		}

		organizationRelationshipQuery := &neoism.CypherQuery{
			Statement: `MERGE (fi:Thing {uuid: {uuid}})
					MERGE (orgUpp:Identifier:UPPIdentifier{value:{orgUuid}})
					MERGE (orgUpp)-[:IDENTIFIES]->(o:Thing) ON CREATE SET o.uuid = {orgUuid}
					MERGE (fi)-[:ISSUED_BY]->(o)`,
			Parameters: map[string]interface{}{
				"uuid": fi.UUID,
				"orgUuid": orgUuid,
			},
		}
		queries = append(queries, organizationRelationshipQuery)
	}

	return s.cypherRunner.CypherBatch(queries)
}

func (s service) Delete(uuid string) (bool, error) {
	clearNode := &neoism.CypherQuery{
		Statement: `MATCH (t:Thing {uuid: {uuid}})
				OPTIONAL MATCH (t)-[is:ISSUED_BY]->(org:Thing)
				OPTIONAL MATCH (t)<-[ir:IDENTIFIES]-(i:Identifier)
				REMOVE t:Concept:FinancialInstrument:Equity
				DELETE is, ir, i
				SET t={props}`,
		Parameters: map[string]interface{}{
			"uuid": uuid,
			"props": map[string]interface{}{
				"uuid": uuid,
			},
		},
		IncludeStats: true,
	}

	removeNodeIfUnused := &neoism.CypherQuery{
		Statement: `MATCH (t:Thing {uuid: {uuid}})
				OPTIONAL MATCH (t)-[a]-(x)
				WITH t, count(a) AS relCount
				WHERE relCount = 0
				DELETE t`,
		Parameters: map[string]interface{}{
			"uuid": uuid,
		},
	}

	err := s.cypherRunner.CypherBatch([]*neoism.CypherQuery{clearNode, removeNodeIfUnused})

	stats, err := clearNode.Stats()
	if err != nil {
		return false, err
	}

	var deleted bool
	if stats.ContainsUpdates && stats.LabelsRemoved > 0 {
		deleted = true
	}

	return deleted, err
}

func (s service) Count() (int, error) {
	results := [] struct {
		Count int `json:"count"`
	}{}

	query := &neoism.CypherQuery{
		Statement: `MATCH (fi:FinancialInstrument) return count(fi) as count`,
		Result: &results,
	}
	err := s.cypherRunner.CypherBatch([]*neoism.CypherQuery{query})

	if err != nil {
		return 0, err
	}

	return results[0].Count, nil
}

func (s service) DecodeJSON(dec *json.Decoder) (interface{}, string, error) {
	fi := financialInstrument{}
	err := dec.Decode(&fi)
	return fi, fi.UUID, err
}

func (s service) IDs(f func(id rwapi.IDEntry) (bool, error)) error {

	for skip := 0; ; skip += batchSize {
		results := []rwapi.IDEntry{}
		readQuery := &neoism.CypherQuery{
			Statement: `MATCH (fi:FinancialInstrument) RETURN fi.uuid as id, fi.hash as hash SKIP {skip} LIMIT {limit}`,
			Parameters: map[string]interface{}{
				"limit": batchSize,
				"skip": skip,
			},
			Result: &results,
		}

		if err := s.cypherRunner.CypherBatch([]*neoism.CypherQuery{readQuery}); err != nil {
			return nil
		}
		if len(results) == 0 {
			return nil
		}
		for _, result := range results {
			more, err := f(result)
			if !more || err != nil {
				return err
			}
		}

	}
}

func (s service) Check() error {
	return neoutils.Check(s.cypherRunner)
}

type requestError struct {
	details string
}

func (re requestError) Error() string {
	return "Invalid Request"
}

func (re requestError) InvalidRequestDetails() string {
	return re.details
}