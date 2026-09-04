# Third-party notices

keel includes code translated from Redis 7.2.4, the last release of Redis under
the BSD 3-Clause licence. The files below carry that licence and the copyright
notices of their authors, and are distributed under those terms. Everything
else in this repository is covered by the licence in `LICENSE`.

| File in keel | Translated from | Copyright |
|---|---|---|
| `internal/data_structure/skiplist.go` | `src/t_zset.c` (the `zskiplist` functions) | 2009-2012 Salvatore Sanfilippo, 2009-2012 Pieter Noordhuis |
| `internal/data_structure/geohash.go` | `src/geohash.c`, `src/geohash_helper.c` | 2013-2014 yinqiwen, 2014 Matt Stancliff, 2015-2016 Salvatore Sanfilippo |
| `internal/data_structure/geo.go` | `src/geo.c` (search helpers and geohash string encoding) | 2014 Matt Stancliff, 2015-2016 Salvatore Sanfilippo |

`geohash_helper.c` is itself a C conversion of `geohash_helper.cpp` from the
ardb project by yinqiwen, which is why that name appears in its notice.

## BSD 3-Clause licence

    Redistribution and use in source and binary forms, with or without
    modification, are permitted provided that the following conditions are met:

      * Redistributions of source code must retain the above copyright notice,
        this list of conditions and the following disclaimer.
      * Redistributions in binary form must reproduce the above copyright
        notice, this list of conditions and the following disclaimer in the
        documentation and/or other materials provided with the distribution.
      * Neither the name of Redis nor the names of its contributors may be used
        to endorse or promote products derived from this software without
        specific prior written permission.

    THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
    AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
    IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
    ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
    LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
    CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
    SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
    INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
    CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
    ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
    POSSIBILITY OF SUCH DAMAGE.

The binary distributions (release archives and the container image) include
this file, which is how the second condition is met.
