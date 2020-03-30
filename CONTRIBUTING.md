# Contributing
To develop on this project, please fork and clone the repo. It is now using go modules, no need to clone in your GOPATH. 

```bash
git clone git@github.com:<YOUR_FORK>/k8s-spot-rescheduler .
cd k8s-spot-recheduler
./configure # Configure local tooling - install anything reported as missing
```

The main package is within `rescheduler.go` and an overview of it's operating logic is described in the [Readme](README.md/#operating-logic).

If you want to run the rescheduler locally you must have a valid `kubeconfig` file somewhere on your machine and then run the program with the flag `--running-in-cluster=false`. You can also specify the path to the kube config with `--kubeconfig=/fully/qualified/path`if it differs from ~/.kube/config which is the default. 

## Pull Requests and Issues
We track bugs and issues using Github .

If you find a bug, please open an Issue.

If you want to fix a bug, please fork, fix the bug and open a PR back to this repo.
Please mention the open bug issue number within your PR if applicable.

### Tests
Unit tests are covering the decision making parts of this code and can be run using the built in Go test suite.

To run the tests: `go test ./... --cover`
