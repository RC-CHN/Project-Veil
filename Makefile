.PHONY: build check race windows-check preservation

build:
	python3 tools/build.py

check:
	python3 tools/check.py

race:
	python3 tools/check.py --race

windows-check:
	python3 tools/check.py --windows-only

preservation:
	python3 tools/verify_migration.py
