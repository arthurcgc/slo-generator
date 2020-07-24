package slo

import (
	"fmt"
	"log"
	"strings"
	"time"

	methods "github.com/globocom/slo-generator/methods"
	samples "github.com/globocom/slo-generator/samples"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/rulefmt"
)

var quantiles = []struct {
	name     string
	quantile float64
}{
	{
		name:     "p50",
		quantile: 0.5,
	},
	{
		name:     "p95",
		quantile: 0.95,
	},
	{
		name:     "p99",
		quantile: 0.99,
	},
}

type SLOSpec struct {
	SLOS []SLO
}

type ExprBlock struct {
	AlertMethod string           `yaml:"alertMethod"`
	AlertWindow string           `yaml:"alertWindow"`
	AlertWait   string           `yaml:"alertWait"`
	Windows     []methods.Window `yaml:"windows"`
	Buckets     []string         `yaml:"buckets"` // used to define buckets of histogram when using latency expression
	Expr        string           `yaml:"expr"`
}

func (block *ExprBlock) ComputeExpr(window, le string) string {
	replacer := strings.NewReplacer("$window", window, "$le", le)
	return replacer.Replace(block.Expr)
}

func (block *ExprBlock) ComputeQuantile(window string, quantile float64) string {
	replacer := strings.NewReplacer("$window", window, "$quantile", fmt.Sprintf("%g", quantile))
	return replacer.Replace(block.Expr)
}

type SLO struct {
	Name       string `yaml:"name"`
	Class      string `yaml:"class"`
	Objectives Objectives

	HonorLabels bool `yaml:"honorLabels"`

	TrafficRateRecord     ExprBlock         `yaml:"trafficRateRecord"`
	ErrorRateRecord       ExprBlock         `yaml:"errorRateRecord"`
	LatencyRecord         ExprBlock         `yaml:"latencyRecord"`
	LatencyQuantileRecord ExprBlock         `yaml:"latencyQuantileRecord"`
	Labels                map[string]string `yaml:"labels"`
	Annotations           map[string]string `yaml:"annotations"`
}

type Objectives struct {
	Availability float64                 `yaml:"availability"`
	Latency      []methods.LatencyTarget `yaml:"latency"`
	Window       model.Duration          `yaml:"window"`
}

// LatencyBuckets returns all boundaries of latencies
// is the same boundaries of a prometheus histogram (aka: le) used to calculate latency SLOs
func (o *Objectives) LatencyBuckets() []string {
	latencyBuckets := []string{}

	for _, latencyBucket := range o.Latency {
		latencyBuckets = append(latencyBuckets, latencyBucket.LE)
	}

	return latencyBuckets
}

func (slo *SLO) GenerateAlertRules(sloClass *Class, disableTicket bool) []rulefmt.Rule {
	objectives := slo.Objectives
	if sloClass != nil {
		objectives = sloClass.Objectives
	}

	alertRules := []rulefmt.Rule{}

	if slo.ErrorRateRecord.AlertMethod != "" {
		errorMethod := methods.Get(slo.ErrorRateRecord.AlertMethod)
		if errorMethod == nil {
			log.Panicf("alertMethod %s is not valid", slo.ErrorRateRecord.AlertMethod)
		}

		errorRules, err := errorMethod.AlertForError(&methods.AlertErrorOptions{
			ServiceName:        slo.Name,
			AvailabilityTarget: objectives.Availability,
			SLOWindow:          time.Duration(objectives.Window),
			Windows:            slo.ErrorRateRecord.Windows,
			AlertWindow:        slo.ErrorRateRecord.AlertWindow,
			AlertWait:          slo.ErrorRateRecord.AlertWait,
		})
		if err != nil {
			log.Panicf("Could not generate alert, err: %s", err.Error())
		}
		alertRules = append(alertRules, errorRules...)
	}

	if slo.LatencyRecord.AlertMethod != "" {
		latencyMethod := methods.Get(slo.LatencyRecord.AlertMethod)
		if latencyMethod == nil {
			log.Panicf("alertMethod %s is not valid", slo.LatencyRecord.AlertMethod)
		}

		if objectives.Latency != nil {
			latencyRules, err := latencyMethod.AlertForLatency(&methods.AlertLatencyOptions{
				ServiceName: slo.Name,
				Targets:     objectives.Latency,
				SLOWindow:   time.Duration(objectives.Window),
				Windows:     slo.LatencyRecord.Windows,
				AlertWindow: slo.LatencyRecord.AlertWindow,
				AlertWait:   slo.LatencyRecord.AlertWait,
			})
			if err != nil {
				log.Panicf("Could not generate alert, err: %s", err.Error())
			}
			alertRules = append(alertRules, latencyRules...)
		}
	}

	for _, rule := range alertRules {
		slo.fillMetadata(&rule)
	}

	if disableTicket {
		alertRulesWithoutTicket := []rulefmt.Rule{}

		for _, rule := range alertRules {
			if rule.Labels["severity"] != "ticket" {
				alertRulesWithoutTicket = append(alertRulesWithoutTicket, rule)
			}
		}

		return alertRulesWithoutTicket
	}

	return alertRules
}

func (slo *SLO) fillMetadata(rule *rulefmt.Rule) {
	for label, value := range slo.Labels {
		rule.Labels[label] = value
	}

	for label, value := range slo.Annotations {
		rule.Annotations[label] = value
	}
}

func (slo *SLO) GenerateGroupRules(sloClass *Class, disableTicket bool) []rulefmt.RuleGroup {
	rules := []rulefmt.RuleGroup{}

	objectives := slo.Objectives
	if sloClass != nil {
		objectives = sloClass.Objectives
	}
	latencyBuckets := objectives.LatencyBuckets()
	if len(slo.LatencyRecord.Buckets) > 0 {
		latencyBuckets = slo.LatencyRecord.Buckets
	}

	for _, sample := range samples.DefaultSamples {

		interval, err := model.ParseDuration(sample.Interval)
		if err != nil {
			log.Fatal(err)
		}
		ruleGroup := rulefmt.RuleGroup{
			Name:     "slo:" + slo.Name + ":" + sample.Name,
			Interval: interval,
			Rules:    []rulefmt.Rule{},
		}

		for _, bucket := range sample.Buckets {
			if disableTicket && samples.IsTicketSample(bucket) {
				continue
			}

			ruleGroup.Rules = append(ruleGroup.Rules, slo.generateRules(bucket, latencyBuckets)...)
		}

		if len(ruleGroup.Rules) > 0 {
			rules = append(rules, ruleGroup)
		}
	}

	return rules
}

func (slo *SLO) labels() map[string]string {
	labels := map[string]string{}
	if !slo.HonorLabels {
		labels["service"] = slo.Name
	}
	for key, value := range slo.Labels {
		labels[key] = value
	}
	return labels
}

func (slo *SLO) generateRules(bucket string, latencyBuckets []string) []rulefmt.Rule {
	rules := []rulefmt.Rule{}
	if slo.TrafficRateRecord.Expr != "" {
		trafficRateRecord := rulefmt.Rule{
			Record: "slo:service_traffic:ratio_rate_" + bucket,
			Expr:   slo.TrafficRateRecord.ComputeExpr(bucket, ""),
			Labels: slo.labels(),
		}

		rules = append(rules, trafficRateRecord)
	}

	if slo.ErrorRateRecord.Expr != "" {
		errorRateRecord := rulefmt.Rule{
			Record: "slo:service_errors_total:ratio_rate_" + bucket,
			Expr:   slo.ErrorRateRecord.ComputeExpr(bucket, ""),
			Labels: slo.labels(),
		}

		rules = append(rules, errorRateRecord)
	}

	if slo.LatencyQuantileRecord.Expr != "" {
		for _, quantile := range quantiles {
			latencyQuantileRecord := rulefmt.Rule{
				Record: "slo:service_latency:" + quantile.name + "_" + bucket,
				Expr:   slo.LatencyQuantileRecord.ComputeQuantile(bucket, quantile.quantile),
				Labels: slo.labels(),
			}

			rules = append(rules, latencyQuantileRecord)
		}
	}

	if slo.LatencyRecord.Expr != "" {
		for _, latencyBucket := range latencyBuckets {
			latencyRateRecord := rulefmt.Rule{
				Record: "slo:service_latency:ratio_rate_" + bucket,
				Expr:   slo.LatencyRecord.ComputeExpr(bucket, latencyBucket),
				Labels: slo.labels(),
			}

			latencyRateRecord.Labels["le"] = latencyBucket

			rules = append(rules, latencyRateRecord)
		}
	}

	return rules
}
