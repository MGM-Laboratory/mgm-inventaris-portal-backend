# Changelog

## 0.1.0 (2026-11-21)


### Features

* add reset-totp and a format prefix on sealed values ([44359f0](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/44359f0a5b2ac35db87e118452ba9e26a8951a48))
* add the admin sign in, the account routes and the create-admin command ([9a0a48c](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/9a0a48c34e32f3b06277ed038ed4be66161adc23))
* add the container image, its workflows and the deploy files ([586260d](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/586260da6530abb52bcd153a82dbd953ab0f70cf))
* add the Go service skeleton, the first schema and the Go CI ([cc65064](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/cc6506407d041c0570714eb5516d235c80141b4f))
* add the guest session routes, the origin and CSRF checks and the rate limiter ([078277e](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/078277e767b66525628f5af561a7488d036c0d3b))
* **admin:** add reset-totp to remove the second factor, the codes and the sessions of an admin ([a7651a1](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/a7651a16e66111e490871d98c23f5f470e14855c))
* **admin:** add the admin sign in, the account routes, the setup session and create-admin ([8db2ab5](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/8db2ab555af92ce0b30bfe1e501a27b825eed14f))
* **auth:** add Argon2id passwords, TOTP codes and sealed secrets ([5a10b6c](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/5a10b6ce5dcddd412852e383282ba648386078d2))
* **cmd:** add the healthcheck subcommand for the image ([77c0226](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/77c0226a86abcd61ac28e21f8d0cea563847925a))
* **config:** add the admin sign in limits, the hash concurrency and the TOTP key settings ([8f0b2e3](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/8f0b2e3c15c7b0408b78999a7dc1adcd5c9f5661))
* **config:** add the guest sign in limit setting ([04db477](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/04db4770f439ffc773804bb06fc868c8f1c3fca0))
* **config:** load the settings from the environment with redacted secrets ([121f86a](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/121f86ad8425ed20e11572691d2b8045a2412c70))
* **config:** trust a list of proxy addresses and CIDR ranges ([8fdc448](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/8fdc44856ddff1e3c90c53883022d01e4ee05eee))
* **database:** add the pool, the migration runner and the first schema ([bd8c678](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/bd8c67884b3b4b8fd00cbe1d842e5c41dd2054a6))
* **database:** refuse a database that is ahead of the binary ([43d06f1](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/43d06f1229e011e0553b18a61606ed90e01dae7a))
* **janitor:** delete expired sessions and old counters under an advisory lock ([08485c6](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/08485c6353f8282b540b086cf3f498a944472846))
* **ratelimit:** count requests in fixed windows with one atomic statement ([22b368b](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/22b368b2c10cf6cb120500d82f62d117d037ddf3))
* **secretbox:** start a sealed value with a format prefix ([a265f2d](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/a265f2de2de90238a5a2978f5154e1e712c92af5))
* **server:** add the API server, the error body, the config route and the health routes ([b953ccb](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/b953ccb5fee79cae8675f2e917ea15dfba20ca2b))
* **server:** add the guest session routes, the origin and CSRF checks and the role guards ([fe35343](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/fe35343460c0e37794e661c19c1fea08be2015e0))
* **server:** cache the readiness ping, log the error chain and drain on shutdown ([6b9ea7f](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/6b9ea7f7cf8e3cdf10801ee47ff3234978028100))
* **session:** store hashed session tokens and build the session cookie ([4d1fc4b](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/4d1fc4b1f3b642334fef2194b6c0dc3423c09fa0))


### Bug Fixes

* **admin:** give only a setup session while the initial password is in force ([febb81f](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/febb81f15173ea8042e7f89249922220550e285b))
* **cmd:** list every command in the healthcheck usage error ([f62908e](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/f62908ef1af911fefd382788141d569d11fa4d2b))
* **config:** drop the default port of the public URL and warn about an empty proxy list ([e341a0a](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/e341a0ade1792db2475e9f414692a91fe88ce769))
* **config:** reject a URL without a host name and keep rejected values out of the errors ([a7d0c1f](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/a7d0c1ff45dc57d363cfd9f67aff2bd5084c9521))
* **database:** bound the migration lock wait and keep the test database off the shared one ([7f9af0b](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/7f9af0b2c87cfcd21c28cd8706d718f600a6ad3d))
* **password:** make the dummy hash at start and fix the window comment ([34ffd37](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/34ffd37bcf2f237fe442d2cedf3719e1e1fab315))
* **server:** answer 500 for a session lookup bug and log errors as a chain without values ([e612054](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/e6120541d5965958da3933ca2a1d5827891406fb))
* **server:** count the email length in characters and trim two comments ([2af88ce](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/2af88ce868b155f3da379ffce7f06905c8f52247))
* **server:** keep the saturation evidence from before the readiness ping ([ea7b535](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/ea7b535321d728780fe2174d06f6c5840d4e8115))
* **server:** label a request that no route matched as unmatched, and use http.StatusOK in a test ([d37c925](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/d37c925923b38549f39670ad98ba57843ab19d1e))
* **server:** refuse a write after a failed lookup and guard every method that is not safe ([d65771d](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/d65771dba2f3cd213f2d22e8325591ea4a9d600b))
* **totp:** remove an unused nolint directive ([4fd5d22](https://github.com/MGM-Laboratory/mgm-inventaris-portal-backend/commit/4fd5d22f698fc48fbbc4eab63c3f37cfe4ccd64d))
