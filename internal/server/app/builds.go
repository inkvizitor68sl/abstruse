package app

import (
	"context"
	"sync"
	"time"

	"github.com/jkuri/abstruse/internal/core"
	"github.com/jkuri/abstruse/internal/pkg/scm"
	"github.com/jkuri/abstruse/internal/server/db/model"
	"github.com/jkuri/abstruse/internal/server/parser"
	jsoniter "github.com/json-iterator/go"
)

// TriggerBuild temp func.
func (app *App) TriggerBuild(repoID, userID uint) error {
	repo, err := app.repoRepository.Find(repoID, userID)
	if err != nil {
		return err
	}
	scm, err := scm.NewSCM(context.Background(), repo.Provider.Name, repo.Provider.URL, repo.Provider.AccessToken)
	if err != nil {
		return err
	}
	commit, err := scm.LastCommit(repo.FullName, repo.DefaultBranch)
	if err != nil {
		return err
	}
	content, err := scm.FindContent(repo.FullName, commit.Sha, ".abstruse.yml")
	if err != nil {
		return err
	}
	config := parser.ConfigParser{Raw: string(content.Data)}
	if err := config.Parse(); err != nil {
		return err
	}
	commandsJSON, err := jsoniter.Marshal(config.Commands)
	if err != nil {
		return err
	}

	buildModel := model.Build{
		Branch:          repo.DefaultBranch,
		Commit:          commit.Sha,
		CommitMessage:   commit.Message,
		Config:          string(content.Data),
		AuthorLogin:     commit.Author.Login,
		AuthorName:      commit.Author.Name,
		AuthorEmail:     commit.Author.Email,
		AuthorAvatar:    commit.Author.Avatar,
		CommitterLogin:  commit.Committer.Name,
		CommitterName:   commit.Committer.Name,
		CommitterEmail:  commit.Committer.Email,
		CommitterAvatar: commit.Committer.Avatar,
		RepositoryID:    repo.ID,
		StartTime:       func(t time.Time) *time.Time { return &t }(time.Now()),
	}
	build, err := app.buildRepository.Create(buildModel)
	if err != nil {
		return err
	}

	for _, env := range config.Env {
		jobModel := &model.Job{
			Image:    config.Parsed.Image,
			Commands: string(commandsJSON),
			Env:      env,
			BuildID:  build.ID,
		}
		job, err := app.jobRepository.Create(jobModel)
		if err != nil {
			return err
		}
		if err := app.scheduleJob(job, repo, commit.Sha); err != nil {
			return err
		}
	}

	if err := app.broadcastNewBuild(build.ID); err != nil {
		return err
	}

	return nil
}

// StopJob stops the job with given id.
func (app *App) StopJob(jobID uint) error {
	return app.scheduler.Cancel(jobID)
}

// RestartJob stops or unqueue the job if running and schedule it again.
func (app *App) RestartJob(jobID uint) error {
	if err := app.StopJob(jobID); err != nil {
		return err
	}
	job, err := app.jobRepository.Find(jobID)
	if err != nil {
		return err
	}
	return app.scheduleJob(job, job.Build.Repository, job.Build.Commit)
}

// StopBuild stops the build and related jobs.
func (app *App) StopBuild(buildID uint) (model.Build, error) {
	build, err := app.buildRepository.FindAll(buildID)
	if err != nil {
		return build, err
	}
	var wg sync.WaitGroup
	wg.Add(len(build.Jobs))
	for _, job := range build.Jobs {
		go func(job *model.Job) {
			if err := app.scheduler.Cancel(job.ID); err != nil {
				app.logger.Errorf("error stopping job %d: %v", job.ID, err)
			}
			wg.Done()
		}(job)
	}
	wg.Wait()
	return build, err
}

// RestartBuild stops the current build related jobs if any, then start them again.
func (app *App) RestartBuild(buildID uint) error {
	build, err := app.StopBuild(buildID)
	if err != nil {
		return err
	}
	build.StartTime = nil
	build.EndTime = nil
	if build, err = app.buildRepository.Update(build); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(len(build.Jobs))
	for _, job := range build.Jobs {
		go func(job *model.Job) {
			if err := app.scheduleJob(job, build.Repository, build.Commit); err != nil {
				app.logger.Debugf("error scheduling job %d: %v", job.ID, err)
			}
			wg.Done()
		}(job)
	}
	wg.Wait()
	return nil
}

func (app *App) scheduleJob(job *model.Job, repo *model.Repository, commitSHA string) error {
	j := &core.Job{
		ID:            job.ID,
		BuildID:       job.BuildID,
		Commands:      job.Commands,
		Image:         job.Image,
		Env:           job.Env,
		URL:           repo.URL,
		ProviderName:  repo.Provider.Name,
		ProviderURL:   repo.Provider.URL,
		ProviderToken: repo.Provider.AccessToken,
		CommitSHA:     commitSHA,
		RepoName:      repo.FullName,
		Priority:      uint16(1000),
		Status:        core.StatusUnknown,
	}
	app.scheduler.Schedule(j)
	return nil
}

func (app *App) updateBuildTime(buildID uint) error {
	build, err := app.buildRepository.FindAll(buildID)
	if err != nil {
		return err
	}
	alldone := true
	var startTime *time.Time
	var endTime *time.Time
	for _, j := range build.Jobs {
		if j.EndTime == nil {
			alldone = false
			break
		} else {
			if endTime == nil || j.EndTime.After(*endTime) {
				endTime = j.EndTime
			}
		}
		if startTime == nil || (j.StartTime != nil && j.StartTime.Before(*startTime)) {
			startTime = j.StartTime
		}
	}
	if startTime != nil {
		build.StartTime = startTime
		if _, err := app.buildRepository.Update(build); err != nil {
			return err
		}
	}
	if alldone {
		build.EndTime = endTime
		if _, err := app.buildRepository.Update(build); err != nil {
			return err
		}
	}
	return nil
}
