THEOS_DEVICE_IP ?=

TARGET := iphone:clang:16.5:16.0
ARCHS = arm64 arm64e

include $(THEOS)/makefiles/common.mk

TWEAK_NAME = RemoteAUCompanion
RemoteAUCompanion_FILES = RemoteAUCompanion.xm
RemoteAUCompanion_FRAMEWORKS = UIKit Foundation AVFoundation
RemoteAUCompanion_CFLAGS = -fobjc-arc

include $(THEOS_MAKE_PATH)/tweak.mk

after-install::
	install.exec "launchctl bootstop gui/501/dev.remoteau.receiver 2>/dev/null || true"
