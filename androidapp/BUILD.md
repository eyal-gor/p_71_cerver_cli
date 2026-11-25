# Building Cerver for Android

The network engine is Go (`../mobile/cervernet`, the same engine as the Mac
service) built into `app/libs/cervernet.aar`. Rebuild it after engine changes:

    export ANDROID_HOME=~/Library/Android/sdk ANDROID_NDK_HOME=$ANDROID_HOME/ndk/27.2.12479018
    cd .. && gomobile bind -target=android/arm64 -androidapi 26 -ldflags="-s -w" \
      -javapkg ai.cerver -o androidapp/app/libs/cervernet.aar ./mobile/cervernet

Then the app:

    JAVA_HOME=/opt/homebrew/opt/openjdk@21 ./gradlew assembleRelease
