# RemoteAU iOS app build helpers.
# Requires: Xcode + XcodeGen (brew install xcodegen)
#
#   make generate   # regenerate RemoteAU.xcodeproj from project.yml
#   make build      # archive Release for generic iOS device
#   make ipa        # unsigned RemoteAU.ipa (sign later or use TrollStore)

PROJECT := RemoteAU.xcodeproj
SCHEME := RemoteAU
BUILD_DIR := build

.PHONY: generate build ipa clean

generate:
	xcodegen generate

build: generate
	xcodebuild -project $(PROJECT) -scheme $(SCHEME) \
		-configuration Release -destination 'generic/platform=iOS' \
		-archivePath $(BUILD_DIR)/RemoteAU.xcarchive archive \
		CODE_SIGNING_ALLOWED=NO CODE_SIGN_IDENTITY=""

ipa: build
	@mkdir -p $(BUILD_DIR)/Payload
	@cp -R "$(BUILD_DIR)/RemoteAU.xcarchive/Products/Applications/RemoteAU.app" $(BUILD_DIR)/Payload/
	@cd $(BUILD_DIR) && rm -f RemoteAU.ipa && zip -qry RemoteAU.ipa Payload
	@rm -rf $(BUILD_DIR)/Payload
	@echo "Built $(BUILD_DIR)/RemoteAU.ipa (unsigned)"

clean:
	rm -rf $(BUILD_DIR) $(PROJECT)
