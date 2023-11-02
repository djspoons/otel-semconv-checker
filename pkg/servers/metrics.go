package servers

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"

	"github.com/madvikinggod/otel-semconv-checker/pkg/semconv"
	pbCollectorMetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	pbMetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type MetricsServer struct {
	pbCollectorMetrics.UnimplementedMetricsServiceServer

	resourceVersion string
	resourceGroups  []string
	resourceIgnore  []string
	matches         []matchDef
	reportUnmatched bool
	oneShot         bool
}

func NewMetricsService(cfg Config, g map[string]semconv.Group) *MetricsServer {
	resourceGroups := []semconv.Group{}
	for _, group := range cfg.Resource.Groups {
		resourceGroups = append(resourceGroups, g[group])
	}
	matches := []matchDef{}
	for _, match := range cfg.Metrics {
		reg := regexp.MustCompile(match.Match)
		groups := []semconv.Group{}
		for _, group := range match.Groups {
			groups = append(groups, g[group])
		}
		matches = append(matches, matchDef{
			name:   reg,
			group:  semconv.GetAttributes(groups...),
			ignore: match.Ignore,
		})
	}

	return &MetricsServer{
		resourceVersion: semconv.Version,
		resourceGroups:  semconv.GetAttributes(resourceGroups...),
		resourceIgnore:  cfg.Resource.Ignore,
		matches:         matches,
		reportUnmatched: cfg.ReportUnmatched,
		oneShot:         cfg.OneShot,
	}
}

func (s *MetricsServer) Export(ctx context.Context, req *pbCollectorMetrics.ExportMetricsServiceRequest) (*pbCollectorMetrics.ExportMetricsServiceResponse, error) {
	if req == nil {
		return nil, nil
	}
	log := slog.With("type", "metrics")
	count := 0
	names := []string{}
	for _, r := range req.ResourceMetrics {
		if r.SchemaUrl != s.resourceVersion {
			log.Info("incorrect resource version",
				slog.String("section", "resource"),
				slog.String("version", r.SchemaUrl),
				slog.String("expected", s.resourceVersion),
			)
		}
		missing, extra := checkResource(s.resourceGroups, s.resourceIgnore, r.Resource)
		logAttributes(log.With(
			slog.String("section", "resource"),
			slog.String("version", r.SchemaUrl),
		), missing, extra)

		for _, scope := range r.ScopeMetrics {
			log := log.With(slog.String("section", "metric"))
			if scope.SchemaUrl != s.resourceVersion {
				log.Info("incorrect scope version",
					slog.String("schemaUrl", scope.SchemaUrl),
					slog.String("expected", s.resourceVersion),
					slog.Any("scope", scope.Scope),
				)
				// count++
			}
			if scope.Scope != nil {
				log = log.With(slog.String("scope.name", scope.Scope.Name))
			}
			fmt.Println(len(scope.Metrics))
			for _, metric := range scope.Metrics {
				found := false
				log := log.With(slog.String("name", metric.Name))
				for _, match := range s.matches {
					if match.name.MatchString(metric.Name) {
						found = true
						missing, extra := checkMetric(log, match.group, match.ignore, metric)
						logAttributes(log, missing, extra)
						count += len(missing)
						names = append(names, scope.Scope.Name)
					}
				}
				if !found && s.reportUnmatched {
					log.Info("unmatched metric")
				}
			}
		}
	}

	if s.oneShot {
		if count > 0 {
			os.Exit(100)
		}
		os.Exit(0)
	}

	if count > 0 {
		return &pbCollectorMetrics.ExportMetricsServiceResponse{
			PartialSuccess: &pbCollectorMetrics.ExportMetricsPartialSuccess{
				RejectedDataPoints: int64(count),
				ErrorMessage:  "missing attributes",
			},
		}, status.Error(codes.FailedPrecondition, fmt.Sprintf("missing attributes: %v", names))
	}

	return &pbCollectorMetrics.ExportMetricsServiceResponse{}, nil
}

func checkMetric(log *slog.Logger, ag, ignore []string, m *pbMetrics.Metric) (missing []string, extra []string) {
	if m == nil {
		return nil, nil
	}

	switch d := m.Data.(type) {
	case *pbMetrics.Metric_Gauge:
		missing, extra = checkNumberDataPoints(ag, ignore, d.Gauge.DataPoints)
	case *pbMetrics.Metric_Sum:
		missing, extra = checkNumberDataPoints(ag, ignore, d.Sum.DataPoints)
		
		// TODO other types
	default:
		log.Warn("Unsupported metric type: %+v", m.Data)
	}
	
	return missing, extra
}

func checkNumberDataPoints(ag, ignore []string, ps []*pbMetrics.NumberDataPoint) (missing []string, extra[] string) {
	for _, p := range ps {
		m, e := semconv.Compare(ag, p.Attributes)
		missing = append(missing, m...)
		extra = append(extra, e...)

	}
	missing, extra = filter(missing, ignore), filter(extra, ignore)
	return missing, extra
}