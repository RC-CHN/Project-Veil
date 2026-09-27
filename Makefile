.PHONY: build prepare test race vet optimized demo clean
build prepare test race vet optimized demo clean:
	$(MAKE) -C veil-core $@

.PHONY: service service-test service-race
service:
	$(MAKE) -C veil-service build
service-test:
	$(MAKE) -C veil-service test
service-race:
	$(MAKE) -C veil-service race
