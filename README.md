# README

#This README contains the steps necessary to get our application up and running.

### What is this repository for? ###

* Quick Summary
* Version

### Summary of setup
* Configuration
* Dependencies
* Database configuration
* How to run tests
* Deployment instructions

### Contribution guidelines ###

* Writing tests
* Code review
* Other guidelines

### Who do I talk to? ###

* Repo owner or admin
* Other community or team contact

### Configure log channels
Debugging issues by tracking logs

There are 4 log channels including console, file, cloudwatch, logstash
Set LOG_DESTINATIONS variable in the `.env` file

ex: `export LOG_DESTINATIONS=file,cloudwatch`

## Mocks

### Generate mocks
If you perform any change in the interfaces in our repository, you need to regenerate the mocks.

```bash
make mock
```

## Linting and pre-commit hook

### Go lint
```bash
sudo snap install golangci-lint
```
Config `.golangci.yaml` file to add or remove lint options

To execute locally run the following command
```bash
make lint
```

### pre-commit hook
```bash
sudo snap install pre-commit --classic
```
Config .pre-commit-config.yaml file to enable or disable pre-commit hook

## Makefile and commands

### Format files
Execute the following command to format the repository to match the standard format from `gopls`.

```bash
make format
```

### Test files
Execute the following command to run the tests in the repository and get a summary of the test results.

```bash
make test
```
