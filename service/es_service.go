package service

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	fthealth "github.com/Financial-Times/go-fthealth/v1_1"
	"github.com/Financial-Times/service-status-go/gtg"
	log "github.com/Sirupsen/logrus"
	"gopkg.in/olivere/elastic.v5"
)

var (
	ErrNoIndexVersion  error = errors.New("No index version has been specified")
	ErrNoElasticClient error = errors.New("No ElasticSearch client available")
)

type EsService interface {
	MigrateIndex(aliasName string, mappingFile string) error
}

type EsHealthService interface {
	GTG() gtg.Status
	ConnectivityHealthyCheck() fthealth.Check
	ClusterIsHealthyCheck() fthealth.Check
	IndexMappingsCheck() fthealth.Check
}

type esService struct {
	sync.RWMutex
	elasticClient       *elastic.Client
	aliasName           string
	mappingFile         string
	aliasFilterFile     string
	indexVersion        string
	pollReindexInterval time.Duration
	progress            string
	migrationCheck      bool
	migrationErr        error
	panicGuideUrl       string
}

func NewEsService(ch chan *elastic.Client, aliasName string, mappingFile string, aliasFilterFile string, indexVersion string, panicGuideUrl string) *esService {
	es := &esService{aliasName: aliasName, mappingFile: mappingFile, aliasFilterFile: aliasFilterFile, indexVersion: indexVersion, pollReindexInterval: time.Minute, progress: "not started", panicGuideUrl: panicGuideUrl}
	go func() {
		for ec := range ch {
			es.setElasticClient(ec)
			es.migrationErr = es.MigrateIndex(es.aliasName, es.mappingFile, es.aliasFilterFile)
			es.migrationCheck = true
		}
	}()
	return es
}

func (es *esService) setElasticClient(ec *elastic.Client) {
	es.Lock()
	defer es.Unlock()

	es.elasticClient = ec
	log.Info("injected ElasticSearch connection")
}

// GTG returns a 503 if the healthcheck fails - suitable for use from varnish to check availability of a node
func (service *esService) GTG() gtg.Status {
	if _, err := service.healthChecker(); err != nil {
		return gtg.Status{GoodToGo: false, Message: err.Error()}
	}
	return gtg.Status{GoodToGo: true}
}

func (es *esService) GetClusterHealth() (*elastic.ClusterHealthResponse, error) {
	es.RLock()
	defer es.RUnlock()

	if err := es.checkElasticClient(); err != nil {
		return nil, err
	}

	return es.elasticClient.ClusterHealth().Do(context.Background())
}

func (es *esService) checkElasticClient() error {
	if es.elasticClient == nil {
		return ErrNoElasticClient
	}

	return nil
}

func (service *esService) esClient() *elastic.Client {
	service.RLock()
	defer service.RUnlock()
	return service.elasticClient
}

func (service *esService) ClusterIsHealthyCheck() fthealth.Check {
	return fthealth.Check{
		BusinessImpact:   "Full or partial degradation in serving requests from Elasticsearch",
		Name:             "Check Elasticsearch cluster health",
		PanicGuide:       service.panicGuideUrl,
		Severity:         2,
		TechnicalSummary: "Elasticsearch cluster is not healthy.",
		Checker:          service.healthChecker,
	}
}

func (service *esService) healthChecker() (string, error) {
	if service.esClient() != nil {
		output, err := service.GetClusterHealth()
		if err != nil {
			return "Cluster is not healthy: ", err
		} else if output.Status != "green" {
			return fmt.Sprintf("Cluster is %v", output.Status), errors.New(fmt.Sprintf("Cluster is %v", output.Status))
		}
		return "Cluster is healthy", nil
	}

	return "Couldn't check the cluster's health.", errors.New("Couldn't establish connectivity.")
}

func (service *esService) ConnectivityHealthyCheck() fthealth.Check {
	return fthealth.Check{
		BusinessImpact:   "Could not connect to Elasticsearch",
		Name:             "Check connectivity to the Elasticsearch cluster",
		PanicGuide:       service.panicGuideUrl,
		Severity:         2,
		TechnicalSummary: "Connection to Elasticsearch cluster could not be created. Please check your AWS credentials.",
		Checker:          service.connectivityChecker,
	}
}

func (service *esService) connectivityChecker() (string, error) {
	if service.esClient() == nil {
		return "", errors.New("Could not connect to elasticsearch, please check the application parameters/env variables, and restart the service.")
	}

	_, err := service.GetClusterHealth()
	if err != nil {
		return "Could not connect to elasticsearch", err
	}
	return "Successfully connected to the cluster", nil
}

func (service *esService) IndexMappingsCheck() fthealth.Check {
	return fthealth.Check{
		BusinessImpact:   "Search results may not be as expected for the data set.",
		Name:             "Check Elasticsearch mappings version",
		PanicGuide:       service.panicGuideUrl,
		Severity:         2,
		TechnicalSummary: "Elasticsearch mappings may not have been migrated.",
		Checker:          service.mappingsChecker,
	}
}

func (service *esService) mappingsChecker() (string, error) {
	if service.migrationErr != nil {
		return "Elasticsearch mappings were not migrated successfully", service.migrationErr
	}

	if !service.migrationCheck {
		msg := fmt.Sprintf("Elasticsearch mappings migration to version %s is in progress (%s)", service.indexVersion, service.progress)
		return msg, errors.New(msg)
	}

	return fmt.Sprintf("Elasticsearch mappings are at version %s", service.indexVersion), nil
}

func (es *esService) MigrateIndex(aliasName string, mappingFile string, aliasFilterFile string) error {
	if len(es.indexVersion) == 0 {
		log.Error(ErrNoIndexVersion.Error())
		return ErrNoIndexVersion
	}

	if _, err := es.healthChecker(); err != nil {
		log.WithError(err).Error("cluster is not healthy")
		return err
	}

	es.progress = "starting"
	client := es.esClient()

	requireUpdate, currentIndexName, newIndexName, err := es.checkIndexAliases(client, aliasName)
	if err != nil {
		log.WithError(err).Error("unable to read alias definition")
		return err
	}
	if !requireUpdate {
		log.WithField("index", es.indexVersion).Info("index is up-to-date")
		return nil
	}

	mapping, err := ioutil.ReadFile(mappingFile)
	if err != nil {
		log.WithError(err).Error("unable to read new index mapping definition")
		return err
	}

	err = es.createIndex(client, newIndexName, string(mapping))
	if err != nil {
		log.WithError(err).Error("unable to create new index")
		return err
	}

	if len(currentIndexName) > 0 {
		err = es.setReadOnly(client, currentIndexName)
		if err != nil {
			log.WithError(err).Error("unable to set index read-only")
			return err
		}

		completeCount, err := es.reindex(client, currentIndexName, newIndexName)
		if err != nil {
			log.WithError(err).Error("failed to begin reindex")
			return err
		}

		taskErrCount := 0
		for {
			finished, done, err := es.isTaskComplete(client, newIndexName, completeCount)
			es.progress = fmt.Sprintf("%v / %v documents reindexed", done, completeCount)
			if err != nil {
				log.WithError(err).Error("failed to obtain reindex task status")
				taskErrCount++
				if taskErrCount == 3 {
					return err
				}
			}

			if finished {
				break
			}

			time.Sleep(es.pollReindexInterval)
		}
	}

	var aliasFilter string
	if len(aliasFilterFile) > 0 {
		aliasFilterBytes, err := ioutil.ReadFile(aliasFilterFile)
		if err != nil {
			log.WithError(err).Error("unable to read alias filter")
			return err
		}
		aliasFilter = string(aliasFilterBytes)
	}

	err = es.updateAlias(client, aliasName, aliasFilter, currentIndexName, newIndexName)
	if err != nil {
		log.WithError(err).Error("failed to update alias")
		return err
	}

	log.WithFields(log.Fields{"from": currentIndexName, "to": newIndexName}).Info("index migration completed")
	return nil
}

func (es *esService) checkIndexAliases(client *elastic.Client, aliasName string) (bool, string, string, error) {
	aliasesService := elastic.NewAliasesService(client)
	aliasesResult, err := aliasesService.Do(context.Background())
	if err != nil {
		return false, "", "", err
	}

	aliasedIndices := aliasesResult.IndicesByAlias(aliasName)
	switch len(aliasedIndices) {
	case 0:
		log.WithField("alias", aliasName).Info("no current index alias")
		requiredIndex := fmt.Sprintf("%s-%s", aliasName, es.indexVersion)

		return true, "", requiredIndex, nil

	case 1:
		log.WithFields(log.Fields{"alias": aliasName, "index": aliasedIndices[0]}).Info("current index alias")
		requiredIndex := fmt.Sprintf("%s-%s", aliasName, es.indexVersion)
		log.WithField("index", requiredIndex).Info("comparing to required index alias")

		return !(aliasedIndices[0] == requiredIndex), aliasedIndices[0], requiredIndex, nil

	default:
		return false, "", "", fmt.Errorf("alias %s points to multiple indices: %v", aliasName, aliasedIndices)
	}
}

func (es *esService) createIndex(client *elastic.Client, indexName string, indexMapping string) error {
	log.WithFields(log.Fields{"indexName": indexName, "mapping": indexMapping}).Info("Creating new index")

	indexService := elastic.NewIndicesCreateService(client)
	_, err := indexService.Index(indexName).BodyString(indexMapping).Do(context.Background())

	return err
}

func (es *esService) setReadOnly(client *elastic.Client, indexName string) error {
	log.WithField("index", indexName).Info("Setting to read-only")

	indexService := elastic.NewIndicesPutSettingsService(client)
	_, err := indexService.Index(indexName).BodyJson(map[string]interface{}{"index.blocks.write": "true"}).Do(context.Background())

	return err
}

func (es *esService) reindex(client *elastic.Client, fromIndex string, toIndex string) (int, error) {
	log.WithFields(log.Fields{"from": fromIndex, "to": toIndex}).Info("reindexing")

	counter := elastic.NewCountService(client)
	count, err := counter.Index(toIndex).Do(context.Background())
	if err != nil {
		return 0, err
	}

	counter = elastic.NewCountService(client)
	count, err = counter.Index(fromIndex).Do(context.Background())
	if err != nil {
		return 0, err
	}

	indexService := elastic.NewReindexService(client)
	_, err = indexService.SourceIndex(fromIndex).DestinationIndex(toIndex).WaitForCompletion(false).Do(context.Background())

	if err != nil {
		return 0, err
	}

	return int(count), err
}

func (es *esService) isTaskComplete(client *elastic.Client, indexName string, completeCount int) (bool, int, error) {
	counter := elastic.NewCountService(client)
	count, err := counter.Index(indexName).Do(context.Background())
	return int(count) == completeCount, int(count), err
}

func (es *esService) updateAlias(client *elastic.Client, aliasName string, aliasFilter string, oldIndexName string, newIndexName string) error {
	log.WithFields(log.Fields{"alias": aliasName, "from": oldIndexName, "to": newIndexName, "filter": aliasFilter}).Info("updating index alias")

	aliasService := elastic.NewAliasService(client)
	if len(oldIndexName) > 0 {
		aliasService = aliasService.Remove(oldIndexName, aliasName)
	}

	if aliasFilter != "" {
		aliasService = aliasService.AddWithFilter(newIndexName, aliasName, elastic.NewRawStringQuery(aliasFilter))
	} else {
		aliasService = aliasService.Add(newIndexName, aliasName)
	}

	_, err := aliasService.Do(context.Background())

	return err
}
