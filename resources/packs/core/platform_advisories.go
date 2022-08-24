package core

import (
	"context"
	"errors"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/rs/zerolog/log"
	"go.mondoo.io/mondoo/logger"
	"go.mondoo.io/mondoo/motor/providers"
	"go.mondoo.io/mondoo/nexus/assets"
	"go.mondoo.io/mondoo/resources"
	"go.mondoo.io/mondoo/vadvisor"
	"go.mondoo.io/mondoo/vadvisor/client"
	"go.mondoo.io/mondoo/vadvisor/specs/cvss"
)

// TODO: generalize this kind of function
func getKernelVersion(kernel Kernel) string {
	raw, err := kernel.Info()
	if err != nil {
		return ""
	}

	info, ok := raw.(map[string]interface{})
	if !ok {
		return ""
	}

	val, ok := info["version"]
	if !ok {
		return ""
	}

	return val.(string)
}

// fetches the vulnerability report and returns the full report
func (p *mqlPlatform) GetVulnerabilityReport() (interface{}, error) {
	r := p.MotorRuntime
	mcc := r.UpstreamConfig
	if mcc == nil {
		return nil, errors.New("mondoo upstream configuration is missing")
	}

	// get platform information
	obj, err := r.CreateResource("platform")
	if err != nil {
		return nil, err
	}

	mqlPlatform := obj.(Platform)
	platformObj := convertMqlPlatform2ApiPlatform(mqlPlatform)

	// check if the data is cached
	// NOTE: we cache it in the platform resource, so that platform.advisories, platform.cves and
	// platform.exploits can all share the results
	cachedReport, ok := mqlPlatform.MqlResource().Cache.Load("_report")
	if ok {
		report := cachedReport.Data.(*vadvisor.VulnReport)
		return report, nil
	}

	// get new advisory report
	// start scanner client
	scannerClient, err := client.New(mcc.Collector, mcc.ApiEndpoint, mcc.Plugins, false, mcc.Incognito)
	if err != nil {
		return nil, err
	}

	asset := &assets.Asset{
		// NOTE: asset mrn may not be available in incognito mode and will be an empty string then
		Mrn:      r.UpstreamConfig.AssetMrn,
		SpaceMrn: r.UpstreamConfig.SpaceMrn,
		Platform: platformObj,
	}

	apiPackages := []*vadvisor.Package{}
	kernelVersion := ""

	// collect pacakges if the platform supports gathering files
	if r.Motor.Provider.Capabilities().HasCapability(providers.Capability_File) {
		obj, err = r.CreateResource("packages")
		if err != nil {
			return nil, err
		}
		packages := obj.(Packages)

		mqlPkgs, err := packages.List()
		if err != nil {
			return nil, err
		}

		for i := range mqlPkgs {
			mqlPkg := mqlPkgs[i]
			pkg := mqlPkg.(Package)
			name, _ := pkg.Name()
			version, _ := pkg.Version()
			arch, _ := pkg.Arch()
			format, _ := pkg.Format()
			origin, _ := pkg.Origin()

			apiPackages = append(apiPackages, &vadvisor.Package{
				Name:    name,
				Version: version,
				Arch:    arch,
				Format:  format,
				Origin:  origin,
			})
		}

		// determine the kernel version if possible (just needed for linux at this point)
		// therefore we ignore the error because its not important, worst case the user sees to many advisories
		objKernel, err := r.CreateResource("kernel")
		if err == nil {
			kernelVersion = getKernelVersion(objKernel.(Kernel))
		}
	}

	scanjob := &vadvisor.AnalyseAssetRequest{
		Platform:      convertPlatform2VulnPlatform(platformObj),
		Packages:      apiPackages,
		KernelVersion: kernelVersion,
	}

	logger.DebugDumpYAML("vuln-scan-job", scanjob)

	log.Debug().Str("asset", asset.Mrn).Bool("incognito", mcc.Incognito).Msg("run advisory scan")
	report, err := scannerClient.AnalysePlatform(context.Background(), scanjob)
	if err != nil {
		return nil, err
	}

	return JsonToDict(report)
}

func getAdvisoryReport(r *resources.Runtime) (*vadvisor.VulnReport, error) {
	obj, err := r.CreateResource("platform")
	if err != nil {
		return nil, err
	}
	platform := obj.(Platform)

	rawReport, err := platform.VulnerabilityReport()
	if err != nil {
		return nil, err
	}

	var vulnReport vadvisor.VulnReport
	cfg := &mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   &vulnReport,
		TagName:  "json",
	}
	decoder, _ := mapstructure.NewDecoder(cfg)
	err = decoder.Decode(rawReport)
	if err != nil {
		return nil, err
	}

	return &vulnReport, nil
}

func (a *mqlPlatformAdvisories) id() (string, error) {
	return "platform.advisories", nil
}

func (a *mqlPlatformAdvisories) GetCvss() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	obj, err := a.MotorRuntime.CreateResource("audit.cvss",
		"score", float64(report.Stats.Score)/10,
		"vector", "", // TODO: we need to extend the report to include the vector in the report
	)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func (a *mqlPlatformAdvisories) GetList() ([]interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	mqlAdvisories := make([]interface{}, len(report.Advisories))
	for i := range report.Advisories {
		advisory := report.Advisories[i]

		var worstScore *cvss.Cvss
		if advisory.WorstScore != nil {
			worstScore = advisory.WorstScore
		} else {
			worstScore = &cvss.Cvss{Score: 0.0, Vector: ""}
		}

		cvssScore, err := a.MotorRuntime.CreateResource("audit.cvss",
			"score", float64(worstScore.Score),
			"vector", worstScore.Vector,
		)
		if err != nil {
			return nil, err
		}

		var published *time.Time
		parsedTime, err := time.Parse(time.RFC3339, advisory.Published)
		if err == nil {
			published = &parsedTime
		}

		var modified *time.Time
		parsedTime, err = time.Parse(time.RFC3339, advisory.Modified)
		if err == nil {
			modified = &parsedTime
		}

		mqlAdvisory, err := a.MotorRuntime.CreateResource("audit.advisory",
			"id", advisory.ID,
			"mrn", advisory.Mrn,
			"title", advisory.Title,
			"description", advisory.Description,
			"published", published,
			"modified", modified,
			"worstScore", cvssScore,
		)
		if err != nil {
			return nil, err
		}

		mqlAdvisories[i] = mqlAdvisory
	}

	return mqlAdvisories, nil
}

func (a *mqlPlatformAdvisories) GetStats() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	dict, err := JsonToDict(report.Stats.Advisories)
	if err != nil {
		return nil, err
	}

	return dict, nil
}

func (a *mqlPlatformCves) id() (string, error) {
	return "platform.cves", nil
}

func (a *mqlPlatformCves) GetList() ([]interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	cveList := report.Cves()

	mqlCves := make([]interface{}, len(cveList))
	for i := range cveList {
		cve := cveList[i]

		var worstScore *cvss.Cvss
		if cve.WorstScore != nil {
			worstScore = cve.WorstScore
		} else {
			worstScore = &cvss.Cvss{Score: 0.0, Vector: ""}
		}

		cvssScore, err := a.MotorRuntime.CreateResource("audit.cvss",
			"score", float64(worstScore.Score),
			"vector", worstScore.Vector,
		)
		if err != nil {
			return nil, err
		}

		var published *time.Time
		parsedTime, err := time.Parse(time.RFC3339, cve.Published)
		if err == nil {
			published = &parsedTime
		}

		var modified *time.Time
		parsedTime, err = time.Parse(time.RFC3339, cve.Modified)
		if err == nil {
			modified = &parsedTime
		}

		mqlCve, err := a.MotorRuntime.CreateResource("audit.cve",
			"id", cve.ID,
			"mrn", cve.Mrn,
			"state", cve.State.String(),
			"summary", cve.Summary,
			"unscored", cve.Unscored,
			"published", published,
			"modified", modified,
			"worstScore", cvssScore,
		)
		if err != nil {
			return nil, err
		}

		mqlCves[i] = mqlCve
	}

	return mqlCves, nil
}

func (a *mqlPlatformCves) GetCvss() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	// TODO: we need to distingush between advisory, cve and exploit cvss
	obj, err := a.MotorRuntime.CreateResource("audit.cvss",
		"score", float64(report.Stats.Score)/10,
		"vector", "", // TODO: we need to extend the report to include the vector in the report
	)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func (a *mqlPlatformCves) GetStats() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	dict, err := JsonToDict(report.Stats.Cves)
	if err != nil {
		return nil, err
	}

	return dict, nil
}

func (a *mqlPlatformExploits) id() (string, error) {
	return "platform.exploits", nil
}

func (a *mqlPlatformExploits) GetList() ([]interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	mqlExploits := make([]interface{}, len(report.Exploits))
	for i := range report.Exploits {
		exploit := report.Exploits[i]

		cvssScore, err := a.MotorRuntime.CreateResource("audit.cvss",
			"score", float64(exploit.Score)/10,
			"vector", "", // TODO: we need to extend the report to include the vector in the report
		)
		if err != nil {
			return nil, err
		}

		var modified *time.Time
		parsedTime, err := time.Parse(time.RFC3339, exploit.Modified)
		if err == nil {
			modified = &parsedTime
		}

		mqlExploit, err := a.MotorRuntime.CreateResource("audit.exploit",
			"id", exploit.ID,
			"mrn", exploit.Mrn,
			"modified", modified,
			"worstScore", cvssScore,
		)
		if err != nil {
			return nil, err
		}

		mqlExploits[i] = mqlExploit
	}

	return mqlExploits, nil
}

func (a *mqlPlatformExploits) GetCvss() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	// TODO: this needs to be the exploit worst score
	obj, err := a.MotorRuntime.CreateResource("audit.cvss",
		"score", float64(report.Stats.Score)/10,
		"vector", "", // TODO: we need to extend the report to include the vector in the report
	)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func (a *mqlPlatformExploits) GetStats() (interface{}, error) {
	report, err := getAdvisoryReport(a.MotorRuntime)
	if err != nil {
		return nil, err
	}

	dict, err := JsonToDict(report.Stats.Exploits)
	if err != nil {
		return nil, err
	}

	return dict, nil
}