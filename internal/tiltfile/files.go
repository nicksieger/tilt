package tiltfile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/windmilleng/tilt/internal/k8s"
	tiltfile_io "github.com/windmilleng/tilt/internal/tiltfile/io"
	"github.com/windmilleng/tilt/internal/tiltfile/starkit"
	"github.com/windmilleng/tilt/internal/tiltfile/value"
	"github.com/windmilleng/tilt/pkg/logger"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"
	"go.starlark.net/starlark"

	"github.com/windmilleng/tilt/internal/kustomize"
)

const localLogPrefix = " → "

func (s *tiltfileState) local(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var command string
	err := s.unpackArgs(fn.Name(), args, kwargs, "command", &command)
	if err != nil {
		return nil, err
	}

	s.logger.Infof("local: %s", command)
	out, err := s.execLocalCmd(thread, exec.Command("sh", "-c", command), true)
	if err != nil {
		return nil, err
	}

	return tiltfile_io.NewBlob(out, fmt.Sprintf("local: %s", command)), nil
}

func (s *tiltfileState) execLocalCmd(t *starlark.Thread, c *exec.Cmd, logOutput bool) (string, error) {
	stdout := bytes.NewBuffer(nil)
	stderr := bytes.NewBuffer(nil)

	// TODO(nick): Should this also inject any docker.Env overrides?
	c.Dir = starkit.AbsWorkingDir(t)
	c.Stdout = stdout
	c.Stderr = stderr

	if logOutput {
		logOutput := NewMutexWriter(logger.NewPrefixedWriter(localLogPrefix, s.logger.Writer(logger.InfoLvl)))
		c.Stdout = io.MultiWriter(stdout, logOutput)
		c.Stderr = io.MultiWriter(stderr, logOutput)
	}

	err := c.Run()
	if err != nil {
		// If we already logged the output, we don't need to log it again.
		if logOutput {
			return "", fmt.Errorf("command %q failed.\nerror: %v", c.Args, err)
		}

		errorMessage := fmt.Sprintf("command %q failed.\nerror: %v\nstdout: %q\nstderr: %q", c.Args, err, stdout.String(), stderr.String())
		return "", errors.New(errorMessage)
	}

	if stdout.Len() == 0 && stderr.Len() == 0 {
		s.logger.Infof("%s[no output]", localLogPrefix)
	}

	return stdout.String(), nil
}

func (s *tiltfileState) kustomize(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path starlark.Value
	err := s.unpackArgs(fn.Name(), args, kwargs, "paths", &path)
	if err != nil {
		return nil, err
	}

	kustomizePath, err := value.ValueToAbsPath(thread, path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (paths): %v", err)
	}

	cmd := []string{"kustomize", "build", kustomizePath}

	_, err = exec.LookPath(cmd[0])
	if err != nil {
		s.logger.Infof("Falling back to `kubectl kustomize` since `kubectl` was not found in PATH")
		cmd = []string{"kubectl", "kustomize", kustomizePath}
	}

	yaml, err := s.execLocalCmd(thread, exec.Command(cmd[0], cmd[1:]...), false)
	if err != nil {
		return nil, err
	}
	deps, err := kustomize.Deps(kustomizePath)
	if err != nil {
		return nil, fmt.Errorf("internal error: %v", err)
	}
	for _, d := range deps {
		err := tiltfile_io.RecordReadFile(thread, d)
		if err != nil {
			return nil, err
		}
	}

	return tiltfile_io.NewBlob(yaml, fmt.Sprintf("kustomize: %s", kustomizePath)), nil
}

func (s *tiltfileState) helm(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path starlark.Value
	var name string
	var namespace string
	var valueFilesV starlark.Value
	var setV starlark.Value
	err := s.unpackArgs(fn.Name(), args, kwargs,
		"paths", &path,
		"name?", &name,
		"namespace?", &namespace,
		"values?", &valueFilesV,
		"set?", &setV)
	if err != nil {
		return nil, err
	}

	localPath, err := value.ValueToAbsPath(thread, path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (paths): %v", err)
	}

	valueFiles, ok := value.AsStringOrStringList(valueFilesV)
	if !ok {
		return nil, fmt.Errorf("Argument 'values' must be string or list of strings. Actual: %T",
			valueFilesV)
	}

	set, ok := value.AsStringOrStringList(setV)
	if !ok {
		return nil, fmt.Errorf("Argument 'set' must be string or list of strings. Actual: %T", setV)
	}

	info, err := os.Stat(localPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Could not read Helm chart directory %q: does not exist", localPath)
		}
		return nil, fmt.Errorf("Could not read Helm chart directory %q: %v", localPath, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("helm() may only be called on directories with Chart.yaml: %q", localPath)
	}

	deps, err := localSubchartDependenciesFromPath(localPath)
	if err != nil {
		return nil, err
	}
	for _, d := range deps {
		err = tiltfile_io.RecordReadFile(thread, starkit.AbsPath(thread, d))
		if err != nil {
			return nil, err
		}
	}

	version, err := getHelmVersion()
	if err != nil {
		return nil, err
	}

	var cmd []string

	if version == helmV3 {
		if name != "" {
			cmd = []string{"helm", "template", name, localPath}
		} else {
			cmd = []string{"helm", "template", localPath, "--generate-name"}
		}
	} else {
		cmd = []string{"helm", "template", localPath}
		if name != "" {
			cmd = append(cmd, "--name", name)
		}
	}

	if namespace != "" {
		cmd = append(cmd, "--namespace", namespace)
	}
	for _, valueFile := range valueFiles {
		cmd = append(cmd, "--values", valueFile)
		err := tiltfile_io.RecordReadFile(thread, starkit.AbsPath(thread, valueFile))
		if err != nil {
			return nil, err
		}
	}
	for _, setArg := range set {
		cmd = append(cmd, "--set", setArg)
	}

	s.logger.Infof("Running: %s", cmd)

	stdout, err := s.execLocalCmd(thread, exec.Command(cmd[0], cmd[1:]...), false)
	if err != nil {
		return nil, err
	}

	err = tiltfile_io.RecordReadFile(thread, localPath)
	if err != nil {
		return nil, err
	}

	yaml := filterHelmTestYAML(string(stdout))

	if namespace != "" {
		// helm template --namespace doesn't inject the namespace, nor provide
		// YAML that defines the namespace, so we have to do both ourselves :\
		// https://github.com/helm/helm/issues/5465
		parsed, err := k8s.ParseYAMLFromString(yaml)
		if err != nil {
			return nil, err
		}

		var haveYAMLForNamespace bool
		for i, e := range parsed {
			if e.GVK().Kind == "Namespace" && e.Name() == namespace {
				// Chart already has YAML for the --namespace passed, we don't need to insert it
				haveYAMLForNamespace = true
				continue
			}
			parsed[i] = e.WithNamespace(namespace)
		}

		var entities []k8s.K8sEntity
		if !haveYAMLForNamespace {
			// User is relying on Helm to create the namespace, which it does independent
			// of the YAML it generates, so we need to make sure the new namespace is included
			// in the YAML.
			entities = []k8s.K8sEntity{k8s.NewNamespaceEntity(namespace)}
		}
		entities = append(entities, parsed...)

		yaml, err = k8s.SerializeSpecYAML(entities)
		if err != nil {
			return nil, err
		}
	}

	return tiltfile_io.NewBlob(yaml, fmt.Sprintf("helm: %s", localPath)), nil
}

func (s *tiltfileState) readYaml(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path starlark.String
	var defaultValue starlark.Value
	if err := s.unpackArgs(fn.Name(), args, kwargs, "paths", &path, "default?", &defaultValue); err != nil {
		return nil, err
	}

	localPath, err := value.ValueToAbsPath(thread, path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (paths): %v", err)
	}

	contents, err := tiltfile_io.ReadFile(thread, localPath)
	if err != nil {
		// Return the default value if the file doesn't exist AND a default value was given
		if os.IsNotExist(err) && defaultValue != nil {
			return defaultValue, nil
		}
		return nil, err
	}

	var decodedYAML interface{}
	err = yaml.Unmarshal(contents, &decodedYAML)
	if err != nil {
		return nil, fmt.Errorf("error parsing YAML: %v in %s", err, path.GoString())
	}

	v, err := convertStructuredDataToStarlark(decodedYAML)
	if err != nil {
		return nil, fmt.Errorf("error converting YAML to Starlark: %v in %s", err, path.GoString())
	}
	return v, nil
}

func (s *tiltfileState) decodeJSON(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var jsonString starlark.String
	if err := s.unpackArgs(fn.Name(), args, kwargs, "json", &jsonString); err != nil {
		return nil, err
	}

	var decodedJSON interface{}

	if err := json.Unmarshal([]byte(jsonString), &decodedJSON); err != nil {
		return nil, fmt.Errorf("JSON parsing error: %v in %s", err, jsonString.GoString())
	}

	v, err := convertStructuredDataToStarlark(decodedJSON)
	if err != nil {
		return nil, fmt.Errorf("error converting JSON to Starlark: %v in %s", err, jsonString.GoString())
	}
	return v, nil
}

func (s *tiltfileState) readJson(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path starlark.String
	var defaultValue starlark.Value
	if err := s.unpackArgs(fn.Name(), args, kwargs, "paths", &path, "default?", &defaultValue); err != nil {
		return nil, err
	}

	localPath, err := value.ValueToAbsPath(thread, path)
	if err != nil {
		return nil, fmt.Errorf("Argument 0 (paths): %v", err)
	}

	contents, err := tiltfile_io.ReadFile(thread, localPath)
	if err != nil {
		// Return the default value if the file doesn't exist AND a default value was given
		if os.IsNotExist(err) && defaultValue != nil {
			return defaultValue, nil
		}
		return nil, err
	}

	var decodedJSON interface{}

	if err := json.Unmarshal(contents, &decodedJSON); err != nil {
		return nil, fmt.Errorf("JSON parsing error: %v in %s", err, path.GoString())
	}

	v, err := convertStructuredDataToStarlark(decodedJSON)
	if err != nil {
		return nil, fmt.Errorf("error converting JSON to Starlark: %v in %s", err, path.GoString())
	}
	return v, nil
}

func convertStructuredDataToStarlark(j interface{}) (starlark.Value, error) {
	switch j := j.(type) {
	case bool:
		return starlark.Bool(j), nil
	case string:
		return starlark.String(j), nil
	case float64:
		return starlark.Float(j), nil
	case []interface{}:
		listOfValues := []starlark.Value{}

		for _, v := range j {
			convertedValue, err := convertStructuredDataToStarlark(v)
			if err != nil {
				return nil, err
			}
			listOfValues = append(listOfValues, convertedValue)
		}

		return starlark.NewList(listOfValues), nil
	case map[string]interface{}:
		mapOfValues := &starlark.Dict{}

		for k, v := range j {
			convertedValue, err := convertStructuredDataToStarlark(v)
			if err != nil {
				return nil, err
			}

			err = mapOfValues.SetKey(starlark.String(k), convertedValue)
			if err != nil {
				return nil, err
			}
		}

		return mapOfValues, nil
	case nil:
		return starlark.None, nil
	}

	return nil, errors.New(fmt.Sprintf("Unable to convert json to starlark value, unexpected type %T", j))
}
