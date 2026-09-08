# Changelog

## [1.4.0](https://github.com/tirante-dev/gascurve/compare/v1.3.0...v1.4.0) (2026-09-08)


### Features

* band the network load chart with its per-bucket spread ([#82](https://github.com/tirante-dev/gascurve/issues/82)) ([ad1125a](https://github.com/tirante-dev/gascurve/commit/ad1125ac52f97c04f36ae0053be62063ad669a1b))
* **collector:** backfill history newest first on archive networks ([#78](https://github.com/tirante-dev/gascurve/issues/78)) ([0031339](https://github.com/tirante-dev/gascurve/commit/00313398423083b00c5cd333ce8f2890dcb1613c))
* **collector:** reach genesis, and let a widened backfill depth take effect ([#79](https://github.com/tirante-dev/gascurve/issues/79)) ([25fe560](https://github.com/tirante-dev/gascurve/commit/25fe560421716c52ea52b070d647d3f11c0d89a4))
* **web:** draw the social card from the live gauge when a link is shared ([#83](https://github.com/tirante-dev/gascurve/issues/83)) ([0a14692](https://github.com/tirante-dev/gascurve/commit/0a146927a9972577cdde0206c67757b6f1a69b67))


### Bug Fixes

* **collector:** retry a size-refused batch narrower and yield the owner scan ([#75](https://github.com/tirante-dev/gascurve/issues/75)) ([608a31d](https://github.com/tirante-dev/gascurve/commit/608a31da14775d25636ceecc019b20e89478d0d3))
* complete constraint-change handling in the filler and the page ([#81](https://github.com/tirante-dev/gascurve/issues/81)) ([6385985](https://github.com/tirante-dev/gascurve/commit/6385985113ffeaf2b27ab2b9abcf3971e446c735))
* **web:** print an inexact bips value as off scale, not as 19 digits ([#77](https://github.com/tirante-dev/gascurve/issues/77)) ([fd47921](https://github.com/tirante-dev/gascurve/commit/fd47921efffe4a4780a70f16d9d178e4b2eb3146))
* **web:** stand the network load chart up to the hero's rail ([#84](https://github.com/tirante-dev/gascurve/issues/84)) ([8e853b7](https://github.com/tirante-dev/gascurve/commit/8e853b7e852cf317422cdb4ce423bb39cf2f05e0))

## [1.3.0](https://github.com/tirante-dev/gascurve/compare/v1.2.0...v1.3.0) (2026-09-08)


### Features

* take a network off the site with enabled: false ([#70](https://github.com/tirante-dev/gascurve/issues/70)) ([6abef99](https://github.com/tirante-dev/gascurve/commit/6abef997b174acd59a8221468990535c9e3eefb7))
* **web:** draw the base fee as a lit gauge over a horizon ([#71](https://github.com/tirante-dev/gascurve/issues/71)) ([c00a757](https://github.com/tirante-dev/gascurve/commit/c00a757d91f017114e4a593aff3b5f583e5dd37f))


### Bug Fixes

* **api:** place undated missing ranges by block number ([#64](https://github.com/tirante-dev/gascurve/issues/64)) ([140b744](https://github.com/tirante-dev/gascurve/commit/140b744951d9ef348d777f5dcdbd579e27484da9))
* **collector:** give history work a counted share against a busy fast loop ([#73](https://github.com/tirante-dev/gascurve/issues/73)) ([5784ac9](https://github.com/tirante-dev/gascurve/commit/5784ac94b17478932b023dc34e02f5ee6290f056))
* **collector:** store each block's prediction against its own header ([#65](https://github.com/tirante-dev/gascurve/issues/65)) ([2a4550e](https://github.com/tirante-dev/gascurve/commit/2a4550e6a9fc4bbcc003aaab01fc4f6d8d6d037f))
* **web:** shrink charts to fit a narrow card instead of scrolling it sideways ([#66](https://github.com/tirante-dev/gascurve/issues/66)) ([c5f5fd0](https://github.com/tirante-dev/gascurve/commit/c5f5fd004de34f5c890a7ff200bb897a0ef1ffc6))


### Performance Improvements

* **web:** draw gap, partial and missing bands as one layer per chart ([#63](https://github.com/tirante-dev/gascurve/issues/63)) ([a1d0a46](https://github.com/tirante-dev/gascurve/commit/a1d0a4662c408003ee91fa17df105d45fa4ebd68))
* **web:** keep feed and ticker re-renders out of the history charts ([#61](https://github.com/tirante-dev/gascurve/issues/61)) ([24d37c5](https://github.com/tirante-dev/gascurve/commit/24d37c5341cdf80df1d7a09056460b617b3f88fa))
* **web:** publish eased live values at 30 Hz instead of every frame ([#62](https://github.com/tirante-dev/gascurve/issues/62)) ([e782c89](https://github.com/tirante-dev/gascurve/commit/e782c899b2d9ec54c19d336ab4306e0b2a6e2cf3))

## [1.2.0](https://github.com/tirante-dev/gascurve/compare/v1.1.0...v1.2.0) (2026-09-07)


### Features

* **web:** define bips on hover ([#52](https://github.com/tirante-dev/gascurve/issues/52)) ([11f4d94](https://github.com/tirante-dev/gascurve/commit/11f4d9450226d01eefe2eb8a45120c0a8597646e))
* **web:** make the live hero readable at a glance ([#57](https://github.com/tirante-dev/gascurve/issues/57)) ([bce2ecb](https://github.com/tirante-dev/gascurve/commit/bce2ecbb214501e83f435382a0fd632dd8e61b47))


### Bug Fixes

* **collector:** backfill poster gas for blocks stored without receipts ([#47](https://github.com/tirante-dev/gascurve/issues/47)) ([8b470f8](https://github.com/tirante-dev/gascurve/commit/8b470f8694b30e4f64039699578e6e78fd335f52))
* **collector:** require a streak of failures to fail readiness ([#48](https://github.com/tirante-dev/gascurve/issues/48)) ([92ae302](https://github.com/tirante-dev/gascurve/commit/92ae3029425637ef2179126866dd015fb7a3de7b))
* **web:** make the USD working discoverable ([#49](https://github.com/tirante-dev/gascurve/issues/49)) ([465ae7a](https://github.com/tirante-dev/gascurve/commit/465ae7a11d671097a08a534e34516211b4a0df16))
* **web:** open the fee flow USD notes as notes, not titles ([#54](https://github.com/tirante-dev/gascurve/issues/54)) ([e470684](https://github.com/tirante-dev/gascurve/commit/e4706846b6af160763829bd4e44990ec2564299f))
* **web:** shade the buckets a series has no data for ([#53](https://github.com/tirante-dev/gascurve/issues/53)) ([522afb8](https://github.com/tirante-dev/gascurve/commit/522afb806d719775cbcf5b8ffe6a834b8666a662))

## [1.1.0](https://github.com/tirante-dev/gascurve/compare/v1.0.1...v1.1.0) (2026-09-07)


### Features

* **collector:** add freshness observability ([#40](https://github.com/tirante-dev/gascurve/issues/40)) ([d8fb93b](https://github.com/tirante-dev/gascurve/commit/d8fb93b80626d53428964cecc07fc07b25e9fabf))
* **collector:** rebuild reconstructed history on a raised history_epoch ([#29](https://github.com/tirante-dev/gascurve/issues/29)) ([0be07c4](https://github.com/tirante-dev/gascurve/commit/0be07c4abc1d7e2ba3c4d18d288b3267b91853c5))
* **web:** lead with Robinhood Chain in search metadata, and ship a real icon set ([#26](https://github.com/tirante-dev/gascurve/issues/26)) ([f6efde9](https://github.com/tirante-dev/gascurve/commit/f6efde93526e374771f5e9c2d981ac53cef629f4))
* **web:** show the math behind every USD figure on hover ([#25](https://github.com/tirante-dev/gascurve/issues/25)) ([ea2b905](https://github.com/tirante-dev/gascurve/commit/ea2b9052260ac59a793d1496fb36648bc811e516))


### Bug Fixes

* account poster gas separately from compute fees ([#45](https://github.com/tirante-dev/gascurve/issues/45)) ([21c8f91](https://github.com/tirante-dev/gascurve/commit/21c8f91306c957802b8a9bc1bdcfdf0a600cf8dc))
* **api:** shed replicas whose notification listener is down ([#38](https://github.com/tirante-dev/gascurve/issues/38)) ([68f0b19](https://github.com/tirante-dev/gascurve/commit/68f0b194b0fef634ca9a0e5ec283446c24af7c16))
* close the gaps an adversarial review found in the metrics ([#36](https://github.com/tirante-dev/gascurve/issues/36)) ([277bc08](https://github.com/tirante-dev/gascurve/commit/277bc087e2189d1f6dec0e1faa1051c68a322dcb))
* **collector:** persist missing ranges durably ([#42](https://github.com/tirante-dev/gascurve/issues/42)) ([72ac271](https://github.com/tirante-dev/gascurve/commit/72ac2714b61b66c677f2f1a0e6667f1d39507f11))
* **collector:** replay owner actions at transaction boundaries ([#41](https://github.com/tirante-dev/gascurve/issues/41)) ([ac422a3](https://github.com/tirante-dev/gascurve/commit/ac422a3ebf6adfc0d24567c84531420bb97e5dc2))
* correct ArbOS batch posting cost accounting ([#44](https://github.com/tirante-dev/gascurve/issues/44)) ([1c1d2d5](https://github.com/tirante-dev/gascurve/commit/1c1d2d542c7084a99689f4aa34be8bbd05a350e2))
* **helm:** preserve client IPs behind ingress ([#37](https://github.com/tirante-dev/gascurve/issues/37)) ([a6b564d](https://github.com/tirante-dev/gascurve/commit/a6b564db928eb99098562a257238569339be9cf6))
* **series:** report incomplete populated buckets ([#43](https://github.com/tirante-dev/gascurve/issues/43)) ([27734f2](https://github.com/tirante-dev/gascurve/commit/27734f226de441806e59184c0cf2cc4bb26a47b9))
* **web:** correct sampling cadence copy ([#31](https://github.com/tirante-dev/gascurve/issues/31)) ([4364f6f](https://github.com/tirante-dev/gascurve/commit/4364f6f16b70eaf0b845a18874c22901872f7ee7))


### Performance Improvements

* **db:** batch block and bucket writes ([#34](https://github.com/tirante-dev/gascurve/issues/34)) ([106e3b3](https://github.com/tirante-dev/gascurve/commit/106e3b3b2e186a9811f1c8d8cac7da64c571ef56))
* **db:** index state sample block queries ([#33](https://github.com/tirante-dev/gascurve/issues/33)) ([61d9cbf](https://github.com/tirante-dev/gascurve/commit/61d9cbfb29f5f4fc1e9fda69205608dd25212e18))

## [1.0.1](https://github.com/tirante-dev/gascurve/compare/v1.0.0...v1.0.1) (2026-09-07)


### Bug Fixes

* **web:** build api and socket URLs from a relative base ([#19](https://github.com/tirante-dev/gascurve/issues/19)) ([425c460](https://github.com/tirante-dev/gascurve/commit/425c4605c9456a89cec4a4c1a0f88b44ace2799f))
* **web:** keep the hero to its charts, inspectors move to the enlarged view ([#20](https://github.com/tirante-dev/gascurve/issues/20)) ([860d541](https://github.com/tirante-dev/gascurve/commit/860d541b0e10f9cb2d2edd7ffb6f02850cc24294))

## 1.0.0 (2026-09-07)


### Features

* initial implementation (collector, api, web, chart) ([#3](https://github.com/tirante-dev/gascurve/issues/3)) ([0f52459](https://github.com/tirante-dev/gascurve/commit/0f524598c3f64a21dbca84415f71f94f6ca03602))


### Bug Fixes

* fourth adversarial review round ([#14](https://github.com/tirante-dev/gascurve/issues/14)) ([b2a0cdd](https://github.com/tirante-dev/gascurve/commit/b2a0cdd4353d4e169e5fccff3dc4d50e32a543eb))


### Dependencies

* Bump golang from 1.26-alpine to 1.27-alpine ([#2](https://github.com/tirante-dev/gascurve/issues/2)) ([afe5630](https://github.com/tirante-dev/gascurve/commit/afe563001c6f98e7f43d319411591db30e4d8785))
* Bump golang from 1.26-alpine to 1.27-alpine ([#5](https://github.com/tirante-dev/gascurve/issues/5)) ([daffa7a](https://github.com/tirante-dev/gascurve/commit/daffa7adbf15f589d921b9c32e37e3c334238492))
* Bump node from 22-alpine to 26-alpine ([#1](https://github.com/tirante-dev/gascurve/issues/1)) ([880115e](https://github.com/tirante-dev/gascurve/commit/880115e0b23a02fb42304b127d6cc3e0d2ef3176))
* Bump node from 22-alpine to 26-alpine ([#4](https://github.com/tirante-dev/gascurve/issues/4)) ([139ee92](https://github.com/tirante-dev/gascurve/commit/139ee92dd1d8fa91c20c8d248103c18fa9f0d541))
* **deps-dev:** Bump @types/node from 22.20.1 to 26.4.1 in /web ([#9](https://github.com/tirante-dev/gascurve/issues/9)) ([bbae034](https://github.com/tirante-dev/gascurve/commit/bbae034348099053327d01b1d800c19f57069026))
* **deps-dev:** bump vitest, jsdom and jest-dom in the web toolchain ([#17](https://github.com/tirante-dev/gascurve/issues/17)) ([424910b](https://github.com/tirante-dev/gascurve/commit/424910b35d370b7c9344b0a740bdfe6c8716581a))

## Changelog
