package operator

import (
	"fmt"

	log "github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	cbTypes "github.com/operator-framework/operator-metering/pkg/apis/metering/v1alpha1"
	"github.com/operator-framework/operator-metering/pkg/db"
	"github.com/operator-framework/operator-metering/pkg/operator/reporting"
	"github.com/operator-framework/operator-metering/pkg/operator/reportingutil"
	"github.com/operator-framework/operator-metering/pkg/presto"
)

func (op *Reporting) runReportGenerationQueryWorker() {
	logger := op.logger.WithField("component", "reportGenerationQueryWorker")
	logger.Infof("ReportGenerationQuery worker started")
	// 10 requeues compared to the 5 others have because
	// ReportGenerationQueries can reference a lot of other resources, and it may
	// take time for them to all to finish setup
	const maxRequeues = 10
	for op.processResource(logger, op.syncReportGenerationQuery, "ReportGenerationQuery", op.reportGenerationQueryQueue, maxRequeues) {
	}
}

func (op *Reporting) syncReportGenerationQuery(logger log.FieldLogger, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.WithError(err).Errorf("invalid resource key :%s", key)
		return nil
	}

	logger = logger.WithField("ReportGenerationQuery", name)

	reportGenerationQueryLister := op.reportGenerationQueryLister
	reportGenerationQuery, err := reportGenerationQueryLister.ReportGenerationQueries(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Infof("ReportGenerationQuery %s does not exist anymore", key)
			return nil
		}
		return err
	}
	q := reportGenerationQuery.DeepCopy()
	return op.handleReportGenerationQuery(logger, q)
}

func (op *Reporting) handleReportGenerationQuery(logger log.FieldLogger, generationQuery *cbTypes.ReportGenerationQuery) error {
	var viewName string
	if generationQuery.Spec.View.Disabled {
		logger.Infof("ReportGenerationQuery has spec.view.disabled=true, skipping view creation")
	} else if generationQuery.Status.ViewName == "" {
		logger.Infof("new ReportGenerationQuery discovered")
		viewName = reportingutil.GenerationQueryViewName(generationQuery.Name)
	} else {
		logger.Infof("existing ReportGenerationQuery discovered, viewName: %s", generationQuery.Status.ViewName)
		viewName = generationQuery.Status.ViewName
	}

	queryDependencies, err := reporting.GetAndValidateGenerationQueryDependencies(
		reporting.NewReportGenerationQueryListerGetter(op.reportGenerationQueryLister),
		reporting.NewReportDataSourceListerGetter(op.reportDataSourceLister),
		reporting.NewReportListerGetter(op.reportLister),
		reporting.NewScheduledReportListerGetter(op.scheduledReportLister),
		generationQuery,
		op.uninitialiedDependendenciesHandler(),
	)
	if err != nil {
		return fmt.Errorf("unable to validate ReportGenerationQuery %s, failed to validate dependencies %v", generationQuery.Name, err)
	}

	if viewName != "" {
		tmplCtx := &reporting.ReportQueryTemplateContext{
			DynamicDependentQueries: queryDependencies.DynamicReportGenerationQueries,
			Report:                  nil,
		}
		renderedQuery, err := reporting.RenderQuery(generationQuery.Spec.Query, tmplCtx)
		if err != nil {
			return err
		}

		err = op.prestoViewCreator.CreateView(viewName, renderedQuery)
		if err != nil {
			return fmt.Errorf("error creating view %s for ReportGenerationQuery %s: %v", viewName, generationQuery.Name, err)
		}

		err = op.updateReportQueryViewName(logger, generationQuery, viewName)
		if err != nil {
			return err
		}
	}

	// enqueue any queries depending on this one
	if err := op.queueDependentReportGenerationQueriesForQuery(generationQuery); err != nil {
		logger.WithError(err).Errorf("error queuing ReportGenerationQuery dependents of ReportGenerationQuery %s", generationQuery.Name)
	}
	// enqueue any reports depending on this one
	if err := op.queueDependentReportsForQuery(generationQuery); err != nil {
		logger.WithError(err).Errorf("error queuing Report dependents of ReportGenerationQuery %s", generationQuery.Name)
	}
	// enqueue any scheduledReports depending on this one
	if err := op.queueDependentScheduledReportsForQuery(generationQuery); err != nil {
		logger.WithError(err).Errorf("error queuing ScheduledReport dependents of ReportGenerationQuery %s", generationQuery.Name)
	}

	return nil
}

func (op *Reporting) updateReportQueryViewName(logger log.FieldLogger, generationQuery *cbTypes.ReportGenerationQuery, viewName string) error {
	generationQuery.Status.ViewName = viewName
	_, err := op.meteringClient.MeteringV1alpha1().ReportGenerationQueries(generationQuery.Namespace).Update(generationQuery)
	if err != nil {
		logger.WithError(err).Errorf("failed to update ReportGenerationQuery view name for %q", generationQuery.Name)
		return err
	}
	return nil
}

func (op *Reporting) uninitialiedDependendenciesHandler() *reporting.UninitialiedDependendenciesHandler {
	return &reporting.UninitialiedDependendenciesHandler{
		HandleUninitializedReportGenerationQuery: op.enqueueReportGenerationQuery,
		HandleUninitializedReportDataSource:      op.enqueueReportDataSource,
	}
}

// queueDependentReportGenerationQueriesForQuery will queue all ReportGenerationQueries in the namespace which have a dependency on the generationQuery
func (op *Reporting) queueDependentReportGenerationQueriesForQuery(generationQuery *cbTypes.ReportGenerationQuery) error {
	queryLister := op.meteringClient.MeteringV1alpha1().ReportGenerationQueries(generationQuery.Namespace)
	queries, err := queryLister.List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, query := range queries.Items {
		// don't queue ourself
		if query.Name == generationQuery.Name {
			continue
		}
		// look at the list of ReportGenerationQuery dependencies
		depenencyNames := append(query.Spec.ReportQueries, query.Spec.DynamicReportQueries...)
		for _, dependency := range depenencyNames {
			if dependency == generationQuery.Name {
				// this query depends on the generationQuery passed in
				op.enqueueReportGenerationQuery(query)
				break
			}
		}
	}
	return nil
}

func (op *Reporting) queueDependentReportsForQuery(generationQuery *cbTypes.ReportGenerationQuery) error {
	reportLister := op.meteringClient.MeteringV1alpha1().Reports(generationQuery.Namespace)
	reports, err := reportLister.List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, report := range reports.Items {
		if report.Spec.GenerationQueryName == generationQuery.Name {
			op.enqueueReport(report)
		}
	}
	return nil
}

func (op *Reporting) queueDependentScheduledReportsForQuery(generationQuery *cbTypes.ReportGenerationQuery) error {
	scheduledReportLister := op.meteringClient.MeteringV1alpha1().ScheduledReports(generationQuery.Namespace)
	scheduledReports, err := scheduledReportLister.List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, report := range scheduledReports.Items {
		if report.Spec.GenerationQueryName == generationQuery.Name {
			op.enqueueScheduledReport(report)
		}
	}
	return nil
}

type PrestoViewCreator interface {
	CreateView(viewName, query string) error
}

type prestoViewCreator struct {
	queryer db.Queryer
}

func (c *prestoViewCreator) CreateView(viewName, query string) error {
	return presto.CreateView(c.queryer, viewName, query, true)
}
