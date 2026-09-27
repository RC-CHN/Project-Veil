BACKEND ?= batch

.PHONY: build test race clean
build:
	python3 scripts/build.py $(BACKEND)
test:
	python3 scripts/build.py $(BACKEND) --check
race:
	python3 scripts/build.py $(BACKEND) --check --race
clean:
	rm -rf -- .build
