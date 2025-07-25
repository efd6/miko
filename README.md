# `miko`

A GUI for running [`mito`](https://pkg.go.dev/github.com/elastic/mito/cmd/mito).

![counting](counting.png)

```
Usage of miko:
  -cfg string
    	path to a YAML file holding run control configuration (see pkg.go.dev/github.com/elastic/mito/cmd/mito)
  -data string
    	path to a JSON object holding input (exposed as the label state)
  -src string
    	path to a CEL program
  -txtar string
    	txtar file containing src.cel, data.json and cfg.yaml (incompatible with any other argument)
```