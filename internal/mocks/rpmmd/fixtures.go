package rpmmd_mock

import (
	"fmt"
	"sort"
	"time"

	"github.com/osbuild/osbuild-composer/internal/jobqueue/fsjobqueue"
	"github.com/osbuild/osbuild-composer/internal/worker"

	"github.com/osbuild/osbuild-composer/internal/rpmmd"
	"github.com/osbuild/osbuild-composer/internal/store"
)

type FixtureGenerator func(tmpdir string) Fixture

func generatePackageList() rpmmd.PackageList {
	baseTime, err := time.Parse(time.RFC3339, "2006-01-02T15:04:05Z")

	if err != nil {
		panic(err)
	}

	var packageList rpmmd.PackageList

	for i := 0; i < 22; i++ {
		basePackage := rpmmd.Package{
			Name:        fmt.Sprintf("package%d", i),
			Summary:     fmt.Sprintf("pkg%d sum", i),
			Description: fmt.Sprintf("pkg%d desc", i),
			URL:         fmt.Sprintf("https://pkg%d.example.com", i),
			Epoch:       0,
			Version:     fmt.Sprintf("%d.0", i),
			Release:     fmt.Sprintf("%d.fc30", i),
			Arch:        "x86_64",
			BuildTime:   baseTime.AddDate(0, i, 0),
			License:     "MIT",
		}

		secondBuild := basePackage

		secondBuild.Version = fmt.Sprintf("%d.1", i)
		secondBuild.BuildTime = basePackage.BuildTime.AddDate(0, 0, 1)

		packageList = append(packageList, basePackage, secondBuild)
	}

	sort.Slice(packageList, func(i, j int) bool {
		return packageList[i].Name < packageList[j].Name
	})

	return packageList
}

func createBaseWorkersFixture(tmpdir string) *worker.Server {
	q, err := fsjobqueue.New(tmpdir)
	if err != nil {
		panic(err)
	}
	return worker.NewServer(nil, q, "")
}

func createBaseDepsolveFixture() []rpmmd.PackageSpec {
	return []rpmmd.PackageSpec{
		{
			Name:    "dep-package3",
			Epoch:   7,
			Version: "3.0.3",
			Release: "1.fc30",
			Arch:    "x86_64",
		},
		{
			Name:    "dep-package1",
			Epoch:   0,
			Version: "1.33",
			Release: "2.fc30",
			Arch:    "x86_64",
		},
		{
			Name:    "dep-package2",
			Epoch:   0,
			Version: "2.9",
			Release: "1.fc30",
			Arch:    "x86_64",
		},
	}
}

func BaseFixture(tmpdir string) Fixture {
	return Fixture{
		fetchPackageList{
			generatePackageList(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		depsolve{
			createBaseDepsolveFixture(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		store.FixtureBase(),
		createBaseWorkersFixture(tmpdir),
	}
}

func NoComposesFixture(tmpdir string) Fixture {
	return Fixture{
		fetchPackageList{
			generatePackageList(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		depsolve{
			createBaseDepsolveFixture(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		store.FixtureEmpty(),
		createBaseWorkersFixture(tmpdir),
	}
}

func NonExistingPackage(tmpdir string) Fixture {
	return Fixture{
		fetchPackageList{
			generatePackageList(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		depsolve{
			nil,
			nil,
			&rpmmd.DNFError{
				Kind:   "MarkingErrors",
				Reason: "Error occurred when marking packages for installation: Problems in request:\nmissing packages: fash",
			},
		},
		store.FixtureBase(),
		createBaseWorkersFixture(tmpdir),
	}
}

func BadDepsolve(tmpdir string) Fixture {
	return Fixture{
		fetchPackageList{
			generatePackageList(),
			map[string]string{"base": "sha256:f34848ca92665c342abd5816c9e3eda0e82180671195362bcd0080544a3bc2ac"},
			nil,
		},
		depsolve{
			nil,
			nil,
			&rpmmd.DNFError{
				Kind:   "DepsolveError",
				Reason: "There was a problem depsolving ['go2rpm']: \n Problem: conflicting requests\n  - nothing provides askalono-cli needed by go2rpm-1-4.fc31.noarch",
			},
		},
		store.FixtureBase(),
		createBaseWorkersFixture(tmpdir),
	}
}

func BadFetch(tmpdir string) Fixture {
	return Fixture{
		fetchPackageList{
			ret:       nil,
			checksums: nil,
			err: &rpmmd.DNFError{
				Kind:   "FetchError",
				Reason: "There was a problem when fetching packages.",
			},
		},
		depsolve{
			nil,
			nil,
			&rpmmd.DNFError{
				Kind:   "DepsolveError",
				Reason: "There was a problem depsolving ['go2rpm']: \n Problem: conflicting requests\n  - nothing provides askalono-cli needed by go2rpm-1-4.fc31.noarch",
			},
		},
		store.FixtureBase(),
		createBaseWorkersFixture(tmpdir),
	}
}
