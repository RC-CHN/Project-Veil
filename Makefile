.PHONY: build prepare test race vet optimized demo clean
build prepare test race vet optimized demo clean:
	$(MAKE) -C veil-core $@
