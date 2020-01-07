package app

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	hcl2 "github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/typeexpr"
	gohcl2 "github.com/hashicorp/hcl/v2/gohcl"
	hcl2parse "github.com/hashicorp/hcl/v2/hclparse"
	"github.com/imdario/mergo"
	"github.com/mumoshu/hcl2test/pkg/conf"
	"github.com/pkg/errors"
	"github.com/variantdev/mod/pkg/shell"
	ctyyaml "github.com/zclconf/go-cty-yaml"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/gocty"
	"gopkg.in/yaml.v3"
)

type hcl2Loader struct {
	Parser *hcl2parse.Parser
}

type configurable struct {
	Body hcl2.Body
}

func (l hcl2Loader) loadFile(filenames ...string) (*configurable, []*hcl2.File, error) {
	var files []*hcl2.File
	var diags hcl2.Diagnostics

	for _, filename := range filenames {
		var f *hcl2.File
		var ds hcl2.Diagnostics
		if strings.HasSuffix(filename, ".json") {
			f, ds = l.Parser.ParseJSONFile(filename)
		} else {
			f, ds = l.Parser.ParseHCLFile(filename)
		}
		files = append(files, f)
		diags = append(diags, ds...)
	}

	if diags.HasErrors() {
		return nil, files, diags
	}

	body := hcl2.MergeFiles(files)

	return &configurable{
		Body: body,
	}, files, nil
}

type Config struct {
	Name string `hcl:"name,label"`

	Sources []Source `hcl:"source,block"`
}

type Source struct {
	Type string `hcl:"type,label"`

	Body hcl2.Body `hcl:",remain"`
}

type SourceFile struct {
	Path    string  `hcl:"path,attr"`
	Default *string `hcl:"default,attr"`
}

type Step struct {
	Name string `hcl:"name,label"`

	Run *RunJob `hcl:"run,block"`

	Needs *[]string `hcl:"need,attr"`
}

type Exec struct {
	Command hcl2.Expression `hcl:"command,attr"`

	Args hcl2.Expression `hcl:"args,attr"`
	Env  hcl2.Expression `hcl:"env,attr"`
}

type RunJob struct {
	Name string `hcl:"name,label"`

	Args map[string]hcl2.Expression `hcl:",remain"`
}

type Parameter struct {
	Name string `hcl:"name,label"`

	Type    hcl2.Expression `hcl:"type,attr"`
	Default hcl2.Expression `hcl:"default,attr"`
	Envs    []EnvSource     `hcl:"env,block"`

	Description *string `hcl:"description,attr"`
}

type EnvSource struct {
	Name string `hcl:"name,label"`
}

type SourceJob struct {
	Name string `hcl:"name,attr"`
	// This results in "no cty.Type for hcl.Expression" error
	//Arguments map[string]hcl2.Expression `hcl:"args,attr"`
	Args   hcl2.Expression `hcl:"args,attr"`
	Format *string         `hcl:"format,attr"`
}

type OptionSpec struct {
	Name string `hcl:"name,label"`

	Type        hcl2.Expression `hcl:"type,attr"`
	Default     hcl2.Expression `hcl:"default,attr"`
	Description *string         `hcl:"description,attr"`
	Short       *string         `hcl:"short,attr"`
}

type Variable struct {
	Name string `hcl:"name,label"`

	Type  hcl2.Expression `hcl:"type,attr"`
	Value hcl2.Expression `hcl:"value,attr"`
}

type JobSpec struct {
	//Type string `hcl:"type,label"`
	Name string `hcl:"name,label"`

	Version *string `hcl:"version,attr"`

	Description *string      `hcl:"description,attr"`
	Parameters  []Parameter  `hcl:"parameter,block"`
	Options     []OptionSpec `hcl:"option,block"`
	Configs     []Config     `hcl:"config,block"`
	Variables   []Variable   `hcl:"variable,block"`

	Concurrency *int `hcl:"concurrency,attr"`

	SourceLocator hcl2.Expression `hcl:"__source_locator,attr"`

	Exec   *Exec           `hcl:"exec,block"`
	Assert []Assert        `hcl:"assert,block"`
	Fail   hcl2.Expression `hcl:"fail,attr"`
	Run    *RunJob         `hcl:"run,block"`

	Steps []Step `hcl:"step,block"'`
}

type Assert struct {
	Name string `hcl:"name,label"`

	Condition hcl2.Expression `hcl:"condition,attr"`
}

type HCL2Config struct {
	Jobs    []JobSpec `hcl:"job,block"`
	Tests   []Test    `hcl:"test,block"`
	JobSpec `hcl:",remain"`
}

type Test struct {
	Name string `hcl:"name,label"`

	Variables []Variable `hcl:"variable,block"`
	Cases     []Case     `hcl:"case,block"`
	Run       RunJob     `hcl:"run,block"`
	Assert    []Assert   `hcl:"assert,block"`

	SourceLocator hcl2.Expression `hcl:"__source_locator,attr"`
}

type Case struct {
	Name string `hcl:"name,label"`

	Args map[string]hcl2.Expression `hcl:",remain"`
}

func (t *configurable) HCL2Config() (*HCL2Config, error) {
	config := &HCL2Config{}

	ctx := &hcl2.EvalContext{
		Functions: conf.Functions("."),
		Variables: map[string]cty.Value{
			"name": cty.StringVal("Ermintrude"),
			"age":  cty.NumberIntVal(32),
			"path": cty.ObjectVal(map[string]cty.Value{
				"root":    cty.StringVal("rootDir"),
				"module":  cty.StringVal("moduleDir"),
				"current": cty.StringVal("currentDir"),
			}),
		},
	}

	diags := gohcl2.DecodeBody(t.Body, ctx, config)
	if diags.HasErrors() {
		// We return the diags as an implementation of error, which the
		// caller than then type-assert if desired to recover the individual
		// diagnostics.
		// FIXME: The current API gives us no way to return warnings in the
		// absence of any errors.
		return config, diags
	}

	return config, nil
}

type App struct {
	Files     map[string]*hcl2.File
	Config    *HCL2Config
	jobByName map[string]JobSpec

	Stdout, Stderr io.Writer

	TraceCommands []string
}

func New(dir string) (*App, error) {
	l := &hcl2Loader{
		Parser: hcl2parse.NewParser(),
	}

	files, err := conf.FindHCLFiles(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to get .hcl files: %v", err)
	}

	//file := "complex.hcl"
	c, hclFiles, err := l.loadFile(files...)
	nameToFiles := map[string]*hcl2.File{}
	for i := range files {
		nameToFiles[files[i]] = hclFiles[i]
	}

	app := &App{
		Files: nameToFiles,
	}
	if err != nil {
		return app, err
	}

	cc, err := c.HCL2Config()
	if err != nil {
		return app, err
	}

	jobByName := map[string]JobSpec{}
	for _, j := range cc.Jobs {
		jobByName[j.Name] = j
	}
	jobByName[""] = cc.JobSpec

	app.Config = cc
	app.jobByName = jobByName

	return app, nil
}

func (app *App) Run(cmd string, args map[string]interface{}, opts map[string]interface{}) (*Result, error) {
	jobByName := app.jobByName
	cc := app.Config

	j, ok := jobByName[cmd]
	if !ok {
		j, ok = jobByName[""]
		if !ok {
			panic(fmt.Errorf("command %q not found", cmd))
		}
	}
	jobCtx, err := createJobContext(cc, j, args, opts)
	if err != nil {
		app.PrintError(err)
		return nil, err
	}

	conf, err := app.getConfigs(jobCtx, cc, j)
	if err != nil {
		return nil, err
	}
	jobCtx.Variables["conf"] = conf

	needs := map[string]cty.Value{}
	res, err := app.execJobSteps(jobCtx, needs, j.Steps)
	if res != nil || err != nil {
		return res, err
	}

	return app.execJob(j, jobCtx)
}

func (app *App) WriteDiags(diagnostics hcl2.Diagnostics) {
	wr := hcl2.NewDiagnosticTextWriter(
		os.Stderr, // writer to send messages to
		app.Files, // the parser's file cache, for source snippets
		100,       // wrapping width
		true,      // generate colored/highlighted output
	)
	wr.WriteDiagnostics(diagnostics)
}

func (app *App) ExitWithError(err error) {
	app.PrintError(err)
	os.Exit(1)
}

func (app *App) PrintError(err error) {
	switch diags := err.(type) {
	case hcl2.Diagnostics:
		app.WriteDiags(diags)
	default:
		fmt.Fprintf(os.Stderr, "%v", err)
	}
}

func (app *App) execCmd(cmd string, args []string, env map[string]string, log bool) (*Result, error) {
	app.TraceCommands = append(app.TraceCommands, fmt.Sprintf("%s %s", cmd, strings.Join(args, " ")))

	shellCmd := &shell.Command{
		Name: cmd,
		Args: args,
		//Stdout: os.Stdout,
		//Stderr: os.Stderr,
		//Stdin:  os.Stdin,
		Env: env,
	}

	sh := shell.Shell{
		Exec: shell.DefaultExec,
	}

	var opts shell.CaptureOpts

	if log {
		opts.LogStdout = func(line string) {
			fmt.Fprintf(app.Stdout, "%s\n", line)
		}
		opts.LogStderr = func(line string) {
			fmt.Fprintf(app.Stderr, "%s\n", line)
		}
	}

	// TODO exec with the runner
	res, err := sh.Capture(shellCmd, opts)

	re := &Result{
		Stdout: res.Stdout,
		Stderr: res.Stderr,
	}

	switch e := err.(type) {
	case *exec.ExitError:
		re.ExitStatus = e.ExitCode()
	}

	if err != nil {
		return re, errors.Wrap(err, app.sanitize(fmt.Sprintf("command \"%s %s\"", cmd, strings.Join(args, " "))))
	}

	return re, nil
}

func (app *App) sanitize(str string) string {
	return str
}

func (app *App) execJob(j JobSpec, ctx *hcl2.EvalContext) (*Result, error) {
	var res *Result
	var err error

	var cmd string
	var args []string
	var env map[string]string
	if j.Exec != nil {
		if diags := gohcl2.DecodeExpression(j.Exec.Command, ctx, &cmd); diags.HasErrors() {
			return nil, diags
		}

		if diags := gohcl2.DecodeExpression(j.Exec.Args, ctx, &args); diags.HasErrors() {
			return nil, diags
		}

		if diags := gohcl2.DecodeExpression(j.Exec.Env, ctx, &env); diags.HasErrors() {
			return nil, diags
		}

		res, err = app.execCmd(cmd, args, env, true)
	} else if j.Run != nil {
		res, err = app.execRun(ctx, j.Run)
	} else if j.Assert != nil {
		for _, a := range j.Assert {
			if err2 := app.execAssert(ctx, a); err2 != nil {
				return nil, err2
			}
		}
		return &Result{}, nil
	}

	if j.Assert != nil && len(j.Assert) > 0 {
		for _, a := range j.Assert {
			if err2 := app.execAssert(ctx, a); err2 != nil {
				return nil, err2
			}
		}
	}

	if res == nil && err == nil {
		res = &Result{}
	}

	return res, err
}

func (app *App) execAssert(ctx *hcl2.EvalContext, a Assert) error {
	var assert bool

	cond := a.Condition

	diags := gohcl2.DecodeExpression(cond, ctx, &assert)
	if diags.HasErrors() {
		return diags
	}

	if !assert {
		fp, err := os.Open(cond.Range().Filename)
		if err != nil {
			panic(err)
		}
		defer fp.Close()

		start := cond.Range().Start.Byte
		b, err := ioutil.ReadAll(fp)
		if err != nil {
			panic(err)
		}
		last := cond.Range().End.Byte + 1
		expr := b[start:last]

		traversals := cond.Variables()
		vars := []string{}
		for _, t := range traversals {
			ctyValue, err := t.TraverseAbs(ctx)
			if err == nil {
				v, err := app.ctyToGo(ctyValue)
				if err != nil {
					panic(err)
				}
				src := strings.TrimSpace(string(b[t.SourceRange().Start.Byte:t.SourceRange().End.Byte]))
				vars = append(vars, fmt.Sprintf("%s=%v (%T)", string(src), v, v))
			}
		}

		return fmt.Errorf("assertion %q failed: this expression must be true, but was false: %s, where %s", a.Name, expr, strings.Join(vars, " "))
	}

	return nil
}

func (app *App) RunTests() (*Result, error) {
	var res *Result
	var err error
	for _, t := range app.Config.Tests {
		res, err = app.execTest(t)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (app *App) execTest(t Test) (*Result, error) {
	var cases []Case
	if len(t.Cases) == 0 {
		cases = []Case{Case{}}
	} else {
		cases = t.Cases
	}
	var res *Result
	var err error
	for _, c := range cases {
		res, err = app.execTestCase(t, c)
		if err != nil {
			return nil, err
		}
	}
	return res, nil
}

func (app *App) execTestCase(t Test, c Case) (*Result, error) {
	ctx := &hcl2.EvalContext{
		Functions: conf.Functions("."),
		Variables: map[string]cty.Value{
			"context": getContext(t.SourceLocator),
		},
	}

	vars, err := getVarialbles(ctx, t.Variables)
	if err != nil {
		return nil, err
	}

	ctx.Variables["var"] = vars

	caseFields := map[string]cty.Value{}
	for k, expr := range c.Args {
		var v cty.Value
		if diags := gohcl2.DecodeExpression(expr, ctx, &v); diags.HasErrors() {
			return nil, diags
		}
		caseFields[k] = v
	}
	caseVal := cty.ObjectVal(caseFields)
	ctx.Variables["case"] = caseVal

	res, err := app.execRun(ctx, &t.Run)

	// If there are one ore more assert(s), do not fail immediately and let the assertion(s) decide
	if t.Assert != nil && len(t.Assert) > 0 {
		var lines []string
		for _, a := range t.Assert {
			if err := app.execAssert(ctx, a); err != nil {
				if strings.HasPrefix(err.Error(), "assertion \"") {
					return nil, fmt.Errorf("case %q: %v", c.Name, err)
				}
				return nil, err
			}
			lines = append(lines, fmt.Sprintf("PASS: %s", a.Name))
		}
		testReport := strings.Join(lines, "\n")
		return &Result{Stdout: testReport}, nil
	}

	return res, err
}

type Result struct {
	Stdout     string
	Stderr     string
	Noop       bool
	ExitStatus int
}

func (res *Result) toCty() cty.Value {
	if res == nil {
		return cty.ObjectVal(map[string]cty.Value{
			"stdout":     cty.StringVal("<not set>"),
			"stderr":     cty.StringVal("<not set>>"),
			"exitstatus": cty.NumberIntVal(int64(-127)),
			"set":        cty.BoolVal(false),
		})
	}
	return cty.ObjectVal(map[string]cty.Value{
		"stdout":     cty.StringVal(res.Stdout),
		"stderr":     cty.StringVal(res.Stderr),
		"exitstatus": cty.NumberIntVal(int64(res.ExitStatus)),
		"set":        cty.BoolVal(true),
	})
}

func (app *App) execRun(jobCtx *hcl2.EvalContext, run *RunJob) (*Result, error) {
	args := map[string]interface{}{}
	for k := range run.Args {
		var v cty.Value
		if diags := gohcl2.DecodeExpression(run.Args[k], jobCtx, &v); diags.HasErrors() {
			return nil, diags
		}
		vv, err := app.ctyToGo(v)
		if err != nil {
			return nil, err
		}
		args[k] = vv
	}

	var err error
	res, err := app.Run(run.Name, args, args)

	runFields := map[string]cty.Value{}
	runFields["res"] = res.toCty()
	if err != nil {
		runFields["err"] = cty.StringVal(err.Error())
	} else {
		runFields["err"] = cty.StringVal("")
	}
	runVal := cty.ObjectVal(runFields)
	jobCtx.Variables["run"] = runVal

	return res, err
}

func (app *App) ctyToGo(v cty.Value) (interface{}, error) {
	var vv interface{}
	switch v.Type() {
	case cty.String:
		var vvv string
		if err := gocty.FromCtyValue(v, &vvv); err != nil {
			return nil, err
		}
		vv = vvv
	case cty.Number:
		var vvv int
		if err := gocty.FromCtyValue(v, &vvv); err != nil {
			return nil, err
		}
		vv = vvv
	case cty.Bool:
		var vvv bool
		if err := gocty.FromCtyValue(v, &vvv); err != nil {
			return nil, err
		}
		vv = vvv
	default:
		return nil, fmt.Errorf("handler for type %s not implemneted yet", v.Type().FriendlyName())
	}

	return vv, nil
}

func (app *App) execJobSteps(jobCtx *hcl2.EvalContext, stepResults map[string]cty.Value, steps []Step) (*Result, error) {
	// TODO Sort steps by name and needs

	// TODO Clone this to avoid mutation
	stepCtx := jobCtx

	var lastRes *Result
	for _, s := range steps {
		var err error
		lastRes, err = app.execRun(stepCtx, s.Run)
		if err != nil {
			return lastRes, err
		}
		stepResults[s.Name] = lastRes.toCty()
		stepResultsVal := cty.ObjectVal(stepResults)
		stepCtx.Variables["step"] = stepResultsVal
	}
	return lastRes, nil
}

func getContext(sourceLocator hcl2.Expression) cty.Value {
	sourcedir := cty.StringVal(filepath.Dir(sourceLocator.Range().Filename))
	context := map[string]cty.Value{}
	{
		context["sourcedir"] = sourcedir
	}
	ctx := cty.ObjectVal(context)
	return ctx
}

func createJobContext(cc *HCL2Config, j JobSpec, givenParams map[string]interface{}, givenOpts map[string]interface{}) (*hcl2.EvalContext, error) {
	ctx := getContext(j.SourceLocator)

	params := map[string]cty.Value{}
	paramSpecs := append(append([]Parameter{}, cc.Parameters...), j.Parameters...)
	for _, p := range paramSpecs {
		v := givenParams[p.Name]

		var tpe cty.Type
		tpe, diags := typeexpr.TypeConstraint(p.Type)
		if diags != nil {
			return nil, diags
		}

		switch v.(type) {
		case nil:
			r := p.Default.Range()
			if r.Start != r.End {
				var vv cty.Value
				defCtx := &hcl2.EvalContext{
					Functions: conf.Functions("."),
					Variables: map[string]cty.Value{
						"context": ctx,
					},
				}
				if err := gohcl2.DecodeExpression(p.Default, defCtx, &vv); err != nil {
					return nil, err
				}
				if vv.Type() != tpe {
					return nil, errors.WithStack(fmt.Errorf("job %q: unexpected type of value %v provided to parameter %q: want %s, got %s", j.Name, vv, p.Name, tpe.FriendlyName(), vv.Type().FriendlyName()))
				}
				params[p.Name] = vv
				continue
			}
			return nil, fmt.Errorf("job %q: missing value for parameter %q", j.Name, p.Name)
		}

		if vty, err := gocty.ImpliedType(v); err != nil {
			return nil, err
		} else if vty != tpe {
			return nil, fmt.Errorf("job %q: unexpected type of option. want %q, got %q", j.Name, tpe.FriendlyNameForConstraint(), vty.FriendlyName())
		}

		val, err := gocty.ToCtyValue(v, tpe)
		if err != nil {
			return nil, err
		}
		params[p.Name] = val
	}

	opts := map[string]cty.Value{}
	optSpecs := append(append([]OptionSpec{}, cc.Options...), j.Options...)
	for _, op := range optSpecs {
		v := givenOpts[op.Name]

		var tpe cty.Type
		tpe, diags := typeexpr.TypeConstraint(op.Type)
		if diags != nil {
			return nil, diags
		}

		switch v.(type) {
		case nil:
			r := op.Default.Range()
			if r.Start != r.End {
				var vv cty.Value
				defCtx := &hcl2.EvalContext{
					Functions: conf.Functions("."),
					Variables: map[string]cty.Value{
						"context": ctx,
					},
				}
				if err := gohcl2.DecodeExpression(op.Default, defCtx, &vv); err != nil {
					return nil, err
				}
				if vv.Type() != tpe {
					return nil, errors.WithStack(fmt.Errorf("job %q: unexpected type of vaule %v provided to option %q: want %s, got %s", j.Name, vv, op.Name, tpe.FriendlyName(), vv.Type().FriendlyName()))
				}
				opts[op.Name] = vv
				continue
			}
			return nil, fmt.Errorf("job %q: missing value for option %q", j.Name, op.Name)
		}

		if vty, err := gocty.ImpliedType(v); err != nil {
			return nil, err
		} else if vty != tpe {
			return nil, fmt.Errorf("job %q: unexpected type of option. want %q, got %q", j.Name, tpe.FriendlyNameForConstraint(), vty.FriendlyName())
		}

		val, err := gocty.ToCtyValue(v, tpe)
		if err != nil {
			return nil, err
		}
		opts[op.Name] = val
	}

	varSpecs := append(append([]Variable{}, cc.Variables...), j.Variables...)
	varCtx := &hcl2.EvalContext{
		Functions: conf.Functions("."),
		Variables: map[string]cty.Value{
			"param":   cty.ObjectVal(params),
			"opt":     cty.ObjectVal(opts),
			"context": ctx,
		},
	}

	vars, err := getVarialbles(varCtx, varSpecs)
	if err != nil {
		return nil, err
	}

	jobCtx := &hcl2.EvalContext{
		Functions: conf.Functions("."),
		Variables: map[string]cty.Value{
			"param":   cty.ObjectVal(params),
			"opt":     cty.ObjectVal(opts),
			"var":     vars,
			"context": ctx,
		},
	}

	return jobCtx, nil
}

func (app *App) getConfigs(confCtx *hcl2.EvalContext, cc *HCL2Config, j JobSpec) (cty.Value, error) {
	confSpecs := append(append([]Config{}, cc.Configs...), j.Configs...)

	confFields := map[string]cty.Value{}
	for confIndex := range confSpecs {
		confSpec := confSpecs[confIndex]
		merged := map[string]interface{}{}
		for sourceIdx := range confSpec.Sources {
			sourceSpec := confSpec.Sources[sourceIdx]
			var yamlData []byte
			var format string

			switch sourceSpec.Type {
			case "file":
				var source SourceFile
				if err := gohcl2.DecodeBody(sourceSpec.Body, confCtx, &source); err != nil {
					return cty.NilVal, err
				}

				var err error
				yamlData, err = ioutil.ReadFile(source.Path)
				if err != nil {
					if source.Default == nil {
						return cty.NilVal, fmt.Errorf("job %q: config %q: source %d: %v", j.Name, confSpec.Name, sourceIdx, err)
					}
					yamlData = []byte(*source.Default)
				}

				format = "yaml"
			case "job":
				var source SourceJob
				if err := gohcl2.DecodeBody(sourceSpec.Body, confCtx, &source); err != nil {
					return cty.NilVal, err
				}

				ctyArgs := map[string]cty.Value{}

				if err := gohcl2.DecodeExpression(source.Args, confCtx, &ctyArgs); err != nil {
					return cty.NilVal, err
				}

				args := map[string]interface{}{}
				for k, v := range ctyArgs {
					vv, err := app.ctyToGo(v)
					if err != nil {
						return cty.NilVal, err
					}
					args[k] = vv
				}

				res, err := app.Run(source.Name, args, args)
				if err != nil {
					return cty.NilVal, err
				}

				yamlData = []byte(res.Stdout)

				if source.Format != nil {
					format = *source.Format
				} else {
					format = "yaml"
				}
			default:
				return cty.DynamicVal, fmt.Errorf("config source %q is not implemented. It must be either \"file\" or \"job\", so that it looks like `source file {` or `source file {`", sourceSpec.Type)
			}

			m := map[string]interface{}{}

			switch format {
			case "yaml":
				if err := yaml.Unmarshal(yamlData, &m); err != nil {
					return cty.NilVal, err
				}
			default:
				return cty.NilVal, fmt.Errorf("format %q is not implemented yet. It must be \"yaml\" or omitted", format)
			}

			if err := mergo.Merge(&merged, m, mergo.WithOverride); err != nil {
				return cty.NilVal, err
			}
		}

		yamlData, err := yaml.Marshal(merged)
		if err != nil {
			return cty.NilVal, err
		}

		ty, err := ctyyaml.ImpliedType(yamlData)
		if err != nil {
			return cty.DynamicVal, err
		}

		v, err := ctyyaml.Unmarshal(yamlData, ty)
		if err != nil {
			return cty.DynamicVal, err
		}

		confFields[confSpec.Name] = v
	}
	return cty.ObjectVal(confFields), nil
}

func getVarialbles(varCtx *hcl2.EvalContext, varSpecs []Variable) (cty.Value, error) {
	varFields := map[string]cty.Value{}
	for _, varSpec := range varSpecs {
		var tpe cty.Type

		if tv, _ := varSpec.Type.Value(nil); !tv.IsNull() {
			var diags hcl2.Diagnostics
			tpe, diags = typeexpr.TypeConstraint(varSpec.Type)
			if diags != nil {
				return cty.ObjectVal(varFields), diags
			}
		}

		if tpe.IsListType() && tpe.ListElementType().Equals(cty.String) {
			var v []string
			if err := gohcl2.DecodeExpression(varSpec.Value, varCtx, &v); err != nil {
				return cty.ObjectVal(varFields), err
			}
			if vty, err := gocty.ImpliedType(v); err != nil {
				return cty.ObjectVal(varFields), err
			} else if vty != tpe {
				return cty.ObjectVal(varFields), fmt.Errorf("unexpected type of option. want %q, got %q", tpe.FriendlyNameForConstraint(), vty.FriendlyName())
			}

			val, err := gocty.ToCtyValue(v, tpe)
			if err != nil {
				return cty.ObjectVal(varFields), err
			}
			varFields[varSpec.Name] = val
		} else {
			var v cty.Value

			if err := gohcl2.DecodeExpression(varSpec.Value, varCtx, &v); err != nil {
				return cty.ObjectVal(varFields), err
			}

			vty := v.Type()

			if !vty.Equals(tpe) {
				return cty.ObjectVal(varFields), fmt.Errorf("unexpected type of value for variable. want %q, got %q", tpe.FriendlyNameForConstraint(), vty.FriendlyName())
			}

			varFields[varSpec.Name] = v
		}
	}
	return cty.ObjectVal(varFields), nil
}
