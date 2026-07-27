// Copyright (C) 2007-2008 Jean-Marc Valin
// Copyright (C) 2008      Thorvald Natvig
// Copyright (C) 2013      Oov
//
// Arbitrary resampling code
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
// 1. Redistributions of source code must retain the above copyright notice,
// this list of conditions and the following disclaimer.
//
// 2. Redistributions in binary form must reproduce the above copyright
// notice, this list of conditions and the following disclaimer in the
// documentation and/or other materials provided with the distribution.
//
// 3. The name of the author may not be used to endorse or promote products
// derived from this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE AUTHOR ``AS IS'' AND ANY EXPRESS OR
// IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES
// OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY DIRECT,
// INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
// SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
// HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
// STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN
// ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
// POSSIBILITY OF SUCH DAMAGE.

package resampler

type kaiserTable struct {
	table      []float64
	oversample int
}

type quality struct {
	baseLength          int
	oversample          int
	downsampleBandwidth float64
	upsampleBandwidth   float64
	table               *kaiserTable
}

var (
	kaiser12 = kaiserTable{
		table: []float64{
			0.99859849, 1.00000000, 0.99859849, 0.99440475, 0.98745105, 0.97779076,
			0.96549770, 0.95066529, 0.93340547, 0.91384741, 0.89213598, 0.86843014,
			0.84290116, 0.81573067, 0.78710866, 0.75723148, 0.72629970, 0.69451601,
			0.66208321, 0.62920216, 0.59606986, 0.56287762, 0.52980938, 0.49704014,
			0.46473455, 0.43304576, 0.40211431, 0.37206735, 0.34301800, 0.31506490,
			0.28829195, 0.26276832, 0.23854851, 0.21567274, 0.19416736, 0.17404546,
			0.15530766, 0.13794294, 0.12192957, 0.10723616, 0.09382272, 0.08164178,
			0.07063950, 0.06075685, 0.05193064, 0.04409466, 0.03718069, 0.03111947,
			0.02584161, 0.02127838, 0.01736250, 0.01402878, 0.01121463, 0.00886058,
			0.00691064, 0.00531256, 0.00401805, 0.00298291, 0.00216702, 0.00153438,
			0.00105297, 0.00069463, 0.00043489, 0.00025272, 0.00013031, 0.0000527734,
			0.00001000, 0.00000000},
		oversample: 64,
	}

	kaiser10 = kaiserTable{
		table: []float64{
			0.99537781, 1.00000000, 0.99537781, 0.98162644, 0.95908712, 0.92831446,
			0.89005583, 0.84522401, 0.79486424, 0.74011713, 0.68217934, 0.62226347,
			0.56155915, 0.50119680, 0.44221549, 0.38553619, 0.33194107, 0.28205962,
			0.23636152, 0.19515633, 0.15859932, 0.12670280, 0.09935205, 0.07632451,
			0.05731132, 0.04193980, 0.02979584, 0.02044510, 0.01345224, 0.00839739,
			0.00488951, 0.00257636, 0.00115101, 0.00035515, 0.00000000, 0.00000000},
		oversample: 32,
	}

	kaiser8 = kaiserTable{
		table: []float64{
			0.99635258, 1.00000000, 0.99635258, 0.98548012, 0.96759014, 0.94302200,
			0.91223751, 0.87580811, 0.83439927, 0.78875245, 0.73966538, 0.68797126,
			0.63451750, 0.58014482, 0.52566725, 0.47185369, 0.41941150, 0.36897272,
			0.32108304, 0.27619388, 0.23465776, 0.19672670, 0.16255380, 0.13219758,
			0.10562887, 0.08273982, 0.06335451, 0.04724088, 0.03412321, 0.02369490,
			0.01563093, 0.00959968, 0.00527363, 0.00233883, 0.00050000, 0.00000000},
		oversample: 32,
	}

	kaiser6 = kaiserTable{
		table: []float64{
			0.99733006, 1.00000000, 0.99733006, 0.98935595, 0.97618418, 0.95799003,
			0.93501423, 0.90755855, 0.87598009, 0.84068475, 0.80211977, 0.76076565,
			0.71712752, 0.67172623, 0.62508937, 0.57774224, 0.53019925, 0.48295561,
			0.43647969, 0.39120616, 0.34752997, 0.30580127, 0.26632152, 0.22934058,
			0.19505503, 0.16360756, 0.13508755, 0.10953262, 0.08693120, 0.06722600,
			0.05031820, 0.03607231, 0.02432151, 0.01487334, 0.00752000, 0.00000000},
		oversample: 32,
	}

	qualityMap = []quality{
		// Q0
		quality{
			baseLength:          8,
			oversample:          4,
			downsampleBandwidth: 0.830,
			upsampleBandwidth:   0.860,
			table:               &kaiser6,
		},
		// Q1
		quality{
			baseLength:          16,
			oversample:          4,
			downsampleBandwidth: 0.850,
			upsampleBandwidth:   0.880,
			table:               &kaiser6,
		},
		// Q2
		quality{
			baseLength:          32,
			oversample:          4,
			downsampleBandwidth: 0.882,
			upsampleBandwidth:   0.910,
			table:               &kaiser6,
		},
		// Q3
		quality{
			baseLength:          48,
			oversample:          8,
			downsampleBandwidth: 0.895,
			upsampleBandwidth:   0.917,
			table:               &kaiser8,
		},
		// Q4
		quality{
			baseLength:          64,
			oversample:          8,
			downsampleBandwidth: 0.921,
			upsampleBandwidth:   0.940,
			table:               &kaiser8,
		},
		// Q5
		quality{
			baseLength:          80,
			oversample:          16,
			downsampleBandwidth: 0.922,
			upsampleBandwidth:   0.940,
			table:               &kaiser10,
		},
		// Q6
		quality{
			baseLength:          96,
			oversample:          16,
			downsampleBandwidth: 0.940,
			upsampleBandwidth:   0.945,
			table:               &kaiser10,
		},
		// Q7
		quality{
			baseLength:          128,
			oversample:          16,
			downsampleBandwidth: 0.950,
			upsampleBandwidth:   0.950,
			table:               &kaiser10,
		},
		// Q8
		quality{
			baseLength:          160,
			oversample:          16,
			downsampleBandwidth: 0.960,
			upsampleBandwidth:   0.960,
			table:               &kaiser10,
		},
		// Q9
		quality{
			baseLength:          192,
			oversample:          32,
			downsampleBandwidth: 0.968,
			upsampleBandwidth:   0.968,
			table:               &kaiser12,
		},
		// Q10
		quality{
			baseLength:          256,
			oversample:          32,
			downsampleBandwidth: 0.975,
			upsampleBandwidth:   0.975,
			table:               &kaiser12,
		},
	}
)
